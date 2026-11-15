package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"toft/internal/runtime"
)

// checkSecurityTxt reads the one date the save path could only check once.
//
// `security_txt_expires` is refused at save if it is already in the past, and
// then it just sits there. The field's own ceiling — a little over two years,
// this platform's opinion rather than the RFC's — exists because "the reminder
// is the entire value of the field". There was no reminder. This is it.
//
// `warn` and never `fail` in both cases: nothing is broken, an install that
// publishes no security.txt at all is a `skip`, and what is being described is
// a promise going stale rather than a system going down.
func (s *Server) checkSecurityTxt(_ context.Context, rp *checkReport) {
	settings, err := s.db.GetPlatformSettings()
	if err != nil {
		rp.add("security_txt", "security.txt", checkWarn, dbReadFailed("security_txt", "the platform settings", err))
		return
	}
	if !settings.SecurityTxtConfigured() {
		rp.add("security_txt", "security.txt", checkSkip,
			"Not published. It is served once a contact is set.")
		return
	}

	raw := strings.TrimSpace(settings.SecurityTxtExpires)
	expires, parseErr := time.Parse("2006-01-02", raw)
	if raw == "" || parseErr != nil {
		// Treated as absent rather than as a finding. The save path is what
		// refuses a bad date, and a check that failed here would be reporting
		// on a state that path does not let anyone reach.
		rp.add("security_txt", "security.txt", checkSkip,
			"Published, with no readable expiry date recorded.")
		return
	}

	today := time.Now().UTC().Truncate(24 * time.Hour)
	when := expires.Format("2 Jan 2006")
	switch {
	case !expires.After(today):
		rp.add("security_txt", "security.txt", checkWarn,
			"Published, and expired on "+when+". A researcher reads an expired file as an abandoned "+
				"one — which is the opposite of what publishing it was for.")
	case expires.Sub(today) < securityTxtExpiryWarning:
		rp.add("security_txt", "security.txt", checkWarn,
			fmt.Sprintf("Published, expiring on %s — in %s. Renew it before a researcher reads it as abandoned.",
				when, humanDuration(expires.Sub(today))))
	default:
		rp.add("security_txt", "security.txt", checkPass, "Published, valid until "+when+".")
	}
}

// checkEncryption is a fact line, and the page says so by never warning.
//
// **This row can never fail, and that is the decision rather than an accident.**
// The two failures worth catching are both refused before this process serves a
// request: a versionless Key Vault URI is rejected at parse, with the reason
// written out, and a malformed key is rejected at ring construction. An install
// that is answering this request has already passed both.
//
// The two remaining candidates are refused here. Scanning stored ciphertexts for
// a key id the ring cannot read means decoding every encrypted column in the
// schema, which is neither cheap nor a fixed list. Probing the KMS is a third
// party on a page that must never be able to hang.
//
// What is left is worth a row anyway: **it names, in one place, the key that has
// to be in the backup.** An install whose database is restored without it holds
// ciphertext nobody can read, and the id is the thing an operator needs and
// would otherwise go looking through configuration for.
func (s *Server) checkEncryption(_ context.Context, rp *checkReport) {
	if s.encryptor == nil {
		rp.add("encryption", "At-rest encryption", checkSkip,
			"This build has no encryptor.")
		return
	}
	held := len(s.encryptor.KeyIDs())
	detail := fmt.Sprintf("Backend %s. New records are written with key %s, and %d key%s %s held for reading. "+
		"That key must be in the backup.",
		s.encryptor.Backend(), s.encryptor.EncryptKeyID(), held, plural(held), pluralIsAre(held))
	// Carries no credential by construction, which is stated where it is
	// returned and is why it may appear on a page a personal key can read.
	if uri := s.encryptor.KEKURI(); uri != "" {
		detail += " Key-encrypting key: " + uri + "."
	}
	rp.add("encryption", "At-rest encryption", checkPass, detail)
}

// checkDevSwitches says the two development switches out loud on an install
// that is not a development one.
//
// `skip` on a loopback install: these are exactly what they are for, and a page
// that amber-lit `scripts/dev.sh` would be teaching every developer to ignore
// its amber before they ever saw a real one.
//
// Not a failure anywhere. The install is doing precisely what it was told, and
// the finding is that somebody told it — this is the built-in administrator
// check's sibling, and it is still the first thing to fix.
func (s *Server) checkDevSwitches(_ context.Context, rp *checkReport) {
	if s.cfg.IsLocalDevelopment() {
		rp.add("dev_switches", "Development switches", checkSkip,
			"This install is loopback-only, which is what the development switches are for.")
		return
	}

	var findings []string
	if s.cfg.DevPassword != "" {
		findings = append(findings, "DEV_PASSWORD is set, which puts one shared password in front of "+
			"every route on an install that is not loopback-only")
	}
	if s.cfg.DevInsecureCookies {
		findings = append(findings, "DEV_INSECURE_COOKIES is set, so session cookies are issued without "+
			"Secure over a network that is not loopback")
	}
	if len(findings) > 0 {
		rp.add("dev_switches", "Development switches", checkWarn, sentence(findings))
		return
	}
	rp.add("dev_switches", "Development switches", checkPass,
		"Neither development switch is set.")
}

// checkTrustedProxy is the one check that reads the request, and it is stated
// here as the exception with its reason: it is the only finding on this page
// that cannot be derived from configuration, because it is about what an actual
// request carried.
//
// The failure it catches is quiet and total. A reverse proxy on another host,
// with TRUSTED_PROXY_CIDRS left at its default, means X-Forwarded-For is
// ignored — so the audit log, the rate limiter and every "who did this" on this
// install record the proxy's address instead of the client's. Nothing errors.
// Everything just says the same IP.
//
// The limit is real and goes in the detail rather than only in a design
// document: this is a finding about *this* request. An administrator sitting on
// the same box as the platform sees a skip while the proxy is misconfigured for
// everybody else — and an administrator coming through the same proxy as
// everyone else is the common case, which is the case it answers.
func (s *Server) checkTrustedProxy(_ context.Context, rp *checkReport) {
	r := rp.req
	if r == nil {
		rp.add("trusted_proxy", "Trusted proxy", checkSkip, "No request to read.")
		return
	}

	forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	switch {
	case viaTrustedProxy(r):
		rp.add("trusted_proxy", "Trusted proxy", checkPass,
			"This request came through a trusted proxy, so its client address is believed.")
	case forwarded != "":
		rp.add("trusted_proxy", "Trusted proxy", checkWarn,
			"X-Forwarded-For was ignored because this request came from outside TRUSTED_PROXY_CIDRS, "+
				"so recorded client addresses are the proxy's. Set TRUSTED_PROXY_CIDRS to the proxy's "+
				"network; this row reads only the request that ran it.")
	default:
		rp.add("trusted_proxy", "Trusted proxy", checkSkip,
			"This request came without a proxy; this row reads only the request that ran it.")
	}
}

// checkHSTS is whether the install tells browsers to use https only, read from
// the same configuration the two routers decide by (security_headers.go).
//
// Unlike trusted_proxy beside it, this reads configuration rather than the
// request, so it says the same thing to every administrator. It reports what
// the configured addresses decide. A request that reaches an http address over
// TLS anyway is still sent the header, and that half is not something an
// install-wide row can speak for.
func (s *Server) checkHSTS(_ context.Context, rp *checkReport) {
	const id, name = "hsts", "HTTPS only (HSTS)"
	cfg := s.cfg
	platform := isHTTPSURL(cfg.BaseURL)
	apps := platform
	if cfg.SeparateAppsListener() {
		apps = isHTTPSURL(cfg.AppsBaseURL)
	}
	switch {
	case hstsWithheld(cfg):
		rp.add(id, name, checkWarn,
			"Not sent: the platform ("+cfg.BaseURL+") and the apps ("+cfg.AppsBaseURL+
				") share a host under different schemes, so HSTS would lock browsers out of the http one. "+
				"Serve both over https, or the apps from their own hostname.")
	case !platform && !apps:
		rp.add(id, name, checkSkip,
			"This install does not serve https, so there is nothing to send.")
	case platform != apps:
		http := "apps"
		if !platform {
			http = "platform"
		}
		rp.add(id, name, checkWarn,
			"Sent on one origin only: the "+http+" address is http. Serve both over https.")
	default:
		rp.add(id, name, checkPass,
			"Sent on every origin, apps included, for one year.")
	}
}

// checkAppContainment states the two facts System info draws in colour about
// how shared apps are held: which account they run as, and whether they can
// reach the cloud metadata endpoints. Before this row the two pages disagreed —
// Admin → System info drew both in red while this report passed and Admin home
// said nothing needed attention — and a page that calls something dangerous
// while the summary above it says all is well teaches its reader to trust
// neither.
//
// Not a "not configured" skip (§30): neither is an optional capability. The
// image runs one account per app by default and the chart blocks the
// endpoints, so this row is quiet on an install set up as shipped and speaks
// only where somebody turned a default off or started the binary bare.
//
//   - Apps running as the platform's own user is `fail`: every app can read the
//     platform's database credentials and every other app's secrets.
//   - Reachable metadata is `warn`: it hands out the machine's cloud identity on
//     a cloud machine and nothing at all on one that is not, and this check
//     cannot tell which it is running on.
func (s *Server) checkAppContainment(_ context.Context, rp *checkReport) {
	switch runtime.AccountsFor(s.cfg.AppRunAsUser, s.cfg.AppUserPerApp) {
	case runtime.AccountsPlatform:
		rp.add("app_accounts", "App accounts", checkFail,
			"Shared apps run as the platform’s own user, so every app can read the platform’s database "+
				"credentials and every other app’s secrets. Set APP_USER_PER_APP=1 and run Toft as root.")
	case runtime.AccountsShared:
		rp.add("app_accounts", "App accounts", checkPass,
			"Shared apps run as one shared account: separated from the platform, not from each other.")
	default:
		rp.add("app_accounts", "App accounts", checkPass,
			"Each shared app runs as its own account.")
	}

	if s.cfg.BlockAWSMetadata {
		rp.add("cloud_metadata", "Cloud metadata endpoints", checkPass,
			"Blocked from apps.")
		return
	}
	rp.add("cloud_metadata", "Cloud metadata endpoints", checkWarn,
		"Apps can reach the cloud metadata endpoints, which on a cloud machine hand out its identity. "+
			"Set BLOCK_AWS_METADATA=1 unless this machine is not in a cloud.")
}
