package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"toft/cli/internal/client"
	"toft/cli/internal/config"
)

const loginUsage = `toft login [<install-url>] — sign this machine in

  $ toft login https://toft.corp

    Install  https://toft.corp
    Code     TFXK-4T2Q

    Opening https://toft.corp/cli …
    (waiting for approval)

    ✓ Signed in as ada@corp.com
      key "toft on ada-mbp" · revoke under Your account → Credentials

The install mints an ordinary personal API key, which you can see and revoke
under Your account → Credentials. Nothing else is stored on this machine.

Flags:
  --status            Who this machine is signed in as, and where
  --forget [install]  Drop a stored credential
  --forget-all        Drop every stored credential
  --token-stdin       Read a key from stdin instead of opening a browser.
                      Takes a personal key or a toftd_ deploy key
  --password-stdin    Sign in with a password read from stdin, and store the
                      session beside the key. Commands use it only when run
                      with --session — the administrator's half of a run:
                      grants, locks, settings, integrations, minting keys
  --user NAME         With --password-stdin: who signs in (default admin)
  --no-browser        Print the URL rather than opening it
  --json              With --status: the current session and every stored one

There is deliberately no --token flag: it would put a credential in your shell
history and in every ps on a shared machine.
`

func cmdLogin(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("login", loginUsage)
	g.register(fs)
	status := fs.Bool("status", false, "who this machine is signed in as")
	forget := fs.String("forget", "", "drop a stored credential")
	forgetAll := fs.Bool("forget-all", false, "drop every stored credential")
	tokenStdin := fs.Bool("token-stdin", false, "read a key from stdin")
	passwordStdin := fs.Bool("password-stdin", false, "sign in with a password read from stdin")
	user := fs.String("user", "admin", "with --password-stdin: who signs in")
	noBrowser := fs.Bool("no-browser", false, "print the URL rather than opening it")
	if err := parseOperands(fs, args, 1); err != nil {
		return skipHelp(err)
	}

	// Built here because `login` is the one command that runs before a session
	// can be resolved, so it never reaches setup(). Guarded the same way setup
	// guards it: a test hands the env a writer of its own beforehand, and
	// replacing it with one pointed at the real terminal would make this the
	// one command whose output no test can read.
	e.buildOut(&g)
	e.json = g.json

	// --status and --forget are questions about the credential rather than
	// things to do, which is why they are flags on `login` and not verbs of
	// their own: `toft logout` implies a session on a server, and there is none.
	switch {
	case *status:
		if g.json {
			return loginStatusJSON(e)
		}
		return loginStatus(e)
	case *forget != "" || *forgetAll:
		return loginForget(e, *forget, *forgetAll)
	}

	installURL := strings.TrimSpace(fs.Arg(0))
	if installURL == "" {
		installURL = g.install
	}
	if installURL == "" {
		// An install already signed in to is the obvious answer to `toft login`
		// with no argument: it is what somebody types when their key expired.
		creds, err := config.LoadCredentials()
		if err != nil {
			return err
		}
		installURL = creds.Default
	}
	if installURL == "" {
		// no_install is exit 3, kind auth, wherever it is said — `toft apps`
		// with nothing stored says it so too (BB-020).
		return authCodef("no_install", "which install? `toft login https://toft.example.com`")
	}
	c, found, err := mustCreds(e).Lookup(installURL)
	if err != nil {
		return err
	}
	if found {
		installURL = c.URL
	}
	// One normalizer, and it is the thing that supplies the scheme:
	// `toft login toft.corp` and `toft login https://toft.corp` are one
	// install because one function says so, not because two places agree.
	installURL = config.NormalizeInstallURL(installURL)
	if !strings.HasPrefix(installURL, "http://") && !strings.HasPrefix(installURL, "https://") {
		// A scheme of its own (ftp://x) is not one to prefix: the suggestion
		// was `https://ftp://x` (BB-020, third 2026-11-03 list).
		if scheme, rest, ok := strings.Cut(installURL, "://"); ok && scheme != "" {
			return usageCodef("invalid_url", "%q is a %s:// address — an install is reached over https:// "+
				"(try `toft login https://%s`)", installURL, scheme, rest)
		}
		return usageCodef("invalid_url", "%q is not a URL and names no stored install — try `toft login https://%s`",
			installURL, installURL)
	}
	if err := refusePlaintext(installURL); err != nil {
		return err
	}

	if *tokenStdin && *passwordStdin {
		return usageCodef("conflicting_flags", "--token-stdin and --password-stdin both read stdin; choose one")
	}
	// The device flow waits for a person to approve it in a browser, so an
	// unattended run refuses rather than hangs, naming the doors that need no
	// person (BB-007). --yes is not unattended here: it answers questions, and
	// the device flow asks none. Before the install probe: a refusal about how
	// the command was run needs nothing from the network.
	if !*tokenStdin && !*passwordStdin &&
		(plainRun(&g) || os.Getenv("CI") != "" || !e.stdinIsTerminal()) {
		return usageCodef("unattended_login", "refusing to start a device sign-in to %s: it waits for a "+
			"person to approve it in a browser, and this run is unattended. Sign in with a key — "+
			"`toft login --token-stdin %s < key.txt` — or a password, `--password-stdin`",
			installURL, installURL)
	}
	// The key and password paths probe the address themselves, after reading
	// and checking what stdin holds: a refusal that needs nothing from the
	// network comes first (BB-054).
	if *tokenStdin {
		return loginWithToken(e, installURL)
	}
	if *passwordStdin {
		return loginWithPassword(e, installURL, *user)
	}
	// Before a device code is asked for: the address has to be an install,
	// not an app (BB-26).
	if err := checkIsInstall(installURL); err != nil {
		return err
	}
	return loginDeviceFlow(e, installURL, *noBrowser)
}

// checkIsInstall refuses an address that is not a Toft install, before a key,
// a password or a device code is sent to it (BB-26).
//
// The address people are actually sent is an app's link, and on it every probe
// the sign-in makes is answered by the app: the gateway proxies
// `/apps/<slug>/api/v1/auth/me` to it, and a catch-all JSON app answers 200 {},
// which stored the key under the app as if it were an install. So the address
// is checked in two ways, both without a credential:
//
//   - a path under the apps prefix is an app's address by its spelling, and is
//     refused without asking anything;
//   - `/api/v1/config` is asked, which only the platform answers — it is
//     unauthenticated because the login page needs it — and an answer that is
//     not the platform's (not 2xx, not JSON, no `version`) is refused, as is
//     one that names this very origin as where apps are served.
//
// Any other path is allowed when the probe at it answers as an install: nothing
// in the product serves the platform under a prefix today, but a front door
// that does is somebody's real setup, and the probe is the evidence that counts.
func checkIsInstall(installURL string) error {
	u, err := url.Parse(installURL)
	if err != nil || u.Host == "" {
		return usageCodef("invalid_url", "%q is not a URL — try `toft login https://toft.example.com`", installURL)
	}
	origin := u.Scheme + "://" + u.Host
	path := strings.Trim(u.Path, "/")
	first, _, _ := strings.Cut(path, "/")

	if first == "apps" {
		// An app's address on either origin. Its root is the install's
		// address on a single-origin install; on the apps origin the probe
		// below cannot name the install, and the hint says so.
		cfg, ok, _ := probeConfig(origin)
		hint := "sign in to the install's own address — the one you open Toft at in a browser — not an app's"
		if ok && !sameOrigin(cfg.AppsBaseURL, origin) {
			hint = "sign in to the install instead: `toft login " + origin + "`"
		}
		return usageHinted("not_an_install", hint,
			"%s is an app's address, not an install's — `toft login` signs in to the install that serves it",
			installURL)
	}

	cfg, ok, unreachable := probeConfig(installURL)
	if unreachable != nil {
		// Not a verdict on what the address is: nothing answered at all.
		return unreachable
	}
	if !ok {
		if path != "" {
			hint := "sign in to the install's own address — the one you open Toft at in a browser"
			if _, rootOK, _ := probeConfig(origin); rootOK {
				hint = "sign in to the install instead: `toft login " + origin + "`"
			}
			return usageHinted("not_an_install", hint,
				"%s did not answer as a Toft install (GET %s/api/v1/config) — an install's address has no path",
				installURL, installURL)
		}
		return usageHinted("not_an_install",
			"if this is where apps are served, sign in to the install's own address instead — the one you open Toft at in a browser",
			"%s did not answer as a Toft install (GET %s/api/v1/config) — it may be where apps are served, or not Toft at all",
			installURL, installURL)
	}
	if cfg.AppsBaseURL != "" && sameOrigin(cfg.AppsBaseURL, origin) {
		return usageHinted("not_an_install",
			"sign in to the install's own address instead — the one you open Toft at in a browser",
			"%s is where this install serves its apps, not the install itself", installURL)
	}
	return nil
}

// probeConfig asks an address for the platform's public configuration, with
// no credential, and reports whether it answered as a Toft install. Nothing
// answering at all is the third result, so an install that is down or
// mistyped is reported as unreachable rather than as something else.
func probeConfig(base string) (installConfig, bool, error) {
	c := client.New(base, "", "toft/"+version)
	hc := *c.HTTP
	hc.Timeout = 30 * time.Second
	resp, err := hc.Get(strings.TrimRight(base, "/") + "/api/v1/config")
	if err != nil {
		return installConfig{}, false, &client.UnreachableError{URL: base, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return installConfig{}, false, nil
	}
	var cfg installConfig
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&cfg); err != nil || cfg.Version == "" {
		return installConfig{}, false, nil
	}
	return cfg, true, nil
}

// sameOrigin reports whether an absolute URL is on the given scheme://host.
func sameOrigin(raw, origin string) bool {
	a, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || a.Host == "" {
		return false
	}
	b, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

// refusePlaintext keeps a credential off the wire in the clear.
//
// A cleartext sign-in carries the device code in one direction and a ninety-day
// key in the other, and anything on the path can read both. The refusal is here,
// before the first request, because every point after it is a point where the
// secret has already been sent.
//
// Loopback is exempt — a development install is the legitimate case and the
// traffic never leaves the machine. It is a host list rather than an --insecure
// flag on purpose: a flag is a thing somebody adds to a CI config once, for a
// reason that stops being true, and never removes.
//
// Only `toft login` refuses. A stored or environment credential for an http://
// install is somebody's existing dev setup, and breaking it here would be a
// change of subject; those commands say one dimmed line at resolution instead.
func refusePlaintext(installURL string) error {
	if !strings.HasPrefix(installURL, "http://") || isLoopback(installURL) {
		return nil
	}
	return usageCodef("insecure_url", "refusing to sign in to %s over plaintext — the device code and the "+
		"key it mints would both be readable in transit. Try https://%s",
		installURL, strings.TrimPrefix(installURL, "http://"))
}

// isLoopback reports whether an install's address never leaves this machine.
func isLoopback(installURL string) bool {
	// Judged on the host net/url will dial, which every request then uses: cut
	// by hand, `localhost:80@evil.example` read as host localhost and port
	// "80@evil.example", passed the plaintext refusal, and dialled
	// evil.example in the clear (code review 2026-11-11, M2-2). Userinfo in
	// an install URL is never loopback.
	if u, err := url.Parse(installURL); err == nil && u.Host != "" {
		if u.User != nil {
			return false
		}
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	}
	host := installURL
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	host, _, _ = strings.Cut(host, "/")
	if strings.Contains(host, "@") {
		return false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// saveCreds writes the credentials file and says so when it had to step over a
// lock left behind by a killed `toft`.
//
// Said rather than swallowed: breaking somebody else's lock is safe here — a
// credentials file is written in milliseconds, so a minute-old lock is a dead
// process, and a file nobody can write is a worse failure than the race — but it
// should appear in the output the one time it turns out to matter.
func saveCreds(e *env, creds *config.CredentialsFile) error {
	if err := creds.Save(); err != nil {
		return err
	}
	if creds.BrokeStaleLock {
		e.out.Dim("Stepped over a stale lock on %s — an toft that was killed left it behind.",
			config.CredentialsPath())
	}
	return nil
}

func mustCreds(e *env) *config.CredentialsFile {
	creds, err := config.LoadCredentials()
	if err != nil {
		return &config.CredentialsFile{Installs: map[string]config.Credential{}}
	}
	return creds
}

// loginDeviceFlow is C5's flow from the client side: ask, show the code, open a
// browser, poll at the interval the server named.
func loginDeviceFlow(e *env, installURL string, noBrowser bool) error {
	c := client.New(installURL, "", "toft/"+version)

	hostname, _ := os.Hostname()
	var started struct {
		DeviceCode       string `json:"device_code"`
		UserCode         string `json:"user_code"`
		VerificationURI  string `json:"verification_uri"`
		VerificationFull string `json:"verification_uri_complete"`
		Interval         int    `json:"interval"`
		ExpiresIn        int    `json:"expires_in"`
	}
	err := c.Do(http.MethodPost, "/api/v1/auth/cli/device", map[string]string{
		"hostname": hostname, "version": version,
	}, &started)
	if err != nil {
		var apiErr *client.Error
		// 404 is "this install does not offer personal API keys" — absent, not
		// refused, which is what the platform means by turning a capability off.
		if errors.As(err, &apiErr) && apiErr.NotFound() {
			return fmt.Errorf("%s does not offer API keys, so there is no way for a "+
				"command-line client to sign in. An administrator turns them on under "+
				"Admin → Settings", installURL)
		}
		return err
	}

	e.out.Say("")
	if e.out.Rich() {
		// The code is the one thing to carry to the browser, so it is the
		// thing framed.
		e.out.Panel(
			e.out.Dimmed(fmt.Sprintf("%-9s", "Install"))+installURL,
			e.out.Dimmed(fmt.Sprintf("%-9s", "Code"))+e.out.Bold(e.out.Accent(started.UserCode)),
		)
	} else {
		e.out.Say("  %-9s%s", "Install", installURL)
		e.out.Say("  %-9s%s", "Code", e.out.Bold(started.UserCode))
	}
	e.out.Say("")

	target := started.VerificationFull
	if target == "" {
		target = started.VerificationURI
	}
	if noBrowser || !browsable(target, installURL) || !openBrowser(target) {
		e.out.Say("  Open %s and enter the code above.", e.out.Accent(target))
	} else {
		e.out.Say("  Opening %s …", e.out.Dimmed(target))
	}
	waiting := e.out.Waiting(e.ctx, "  waiting for approval")
	if waiting == nil {
		e.out.Say("  %s", e.out.Dimmed("(waiting for approval)"))
	}

	approved, err := pollForApproval(e, c, started.DeviceCode, started.Interval)
	waiting.Stop()
	if err != nil {
		return err
	}

	creds, err := config.LoadCredentials()
	if err != nil {
		return err
	}
	// The expiry is already in the response and was once dropped on the
	// floor. A ninety-day key that expires with no warning anywhere is an
	// outage on a schedule.
	setKey(creds, installURL, approved.Token, approved.User, approved.ExpiresAt)
	if err := saveCreds(e, creds); err != nil {
		return fmt.Errorf("saving the credential: %w", err)
	}

	if e.json {
		return signedInDocument(e, installURL, approved.User, approved.KeyName,
			false, approved.ExpiresAt)
	}
	e.out.Say("")
	if e.out.Rich() {
		lines := []string{
			e.out.OK("✓ ") + "Signed in as " + e.out.Bold(approved.User),
			"  " + e.out.Dimmed("on ") + e.out.Link(installURL, installURL),
			"  " + e.out.Dimmed(fmt.Sprintf("key %q · revoke at %s/account/api-keys", approved.KeyName, installURL)),
		}
		if !approved.ExpiresAt.IsZero() {
			lines = append(lines, "  "+e.out.Dimmed("expires "+approved.ExpiresAt.UTC().Format("2006-01-02")+
				" ("+untilWords(approved.ExpiresAt)+")"))
		}
		e.out.Panel(lines...)
		e.out.Next("toft whoami")
		return nil
	}
	e.out.Say("  %s Signed in as %s", e.out.OK("✓"), e.out.Bold(approved.User))
	e.out.Say("    %s", e.out.Dimmed(fmt.Sprintf("key %q · revoke at %s/account/api-keys", approved.KeyName, installURL)))
	if !approved.ExpiresAt.IsZero() {
		e.out.Say("    %s", e.out.Dimmed(fmt.Sprintf("expires %s", approved.ExpiresAt.UTC().Format("2006-01-02"))))
	}
	return nil
}

// signedInDocument is what a program is told when a credential is stored.
//
// **It carries no token and never will.** The credential is on disk, which is
// where it belongs; a caller that wanted it back has the file. What this answers
// is the pair of questions a script actually has — *which install am I now
// bound to, and as whom* — plus the class, because a deploy key belongs to
// nobody and a script that expects a user would otherwise read the empty field
// as an error.
func signedInDocument(e *env, installURL, user, keyName string, deployKey bool,
	expires time.Time) error {

	doc := map[string]any{
		"install":    installURL,
		"deploy_key": deployKey,
	}
	if user != "" {
		doc["user"] = user
	}
	if keyName != "" {
		doc["key_name"] = keyName
	}
	if !expires.IsZero() {
		doc["expires_at"] = expires.UTC()
	}
	return json.NewEncoder(e.out.Stdout).Encode(doc)
}

// approval is what the device flow comes back with. A struct rather than a
// fourth and fifth return value, because a tuple that wide stops saying which
// field is which at the call site.
type approval struct {
	Token     string
	User      string
	KeyName   string
	ExpiresAt time.Time
}

// pollForApproval waits for the human, at the interval the server asked for.
//
// `slow_down` is obeyed by backing off rather than ignored, and it is the one
// answer here that is a 429: a client that hammered a login endpoint would be
// indistinguishable from one attacking it.
func pollForApproval(e *env, c *client.Client, deviceCode string, interval int) (approval, error) {
	if interval < 1 {
		interval = 5
	}
	wait := time.Duration(interval) * time.Second

	for {
		select {
		case <-e.ctx.Done():
			return approval{}, e.ctx.Err()
		case <-time.After(wait):
		}

		var answer struct {
			Status string `json:"status"`
			Token  string `json:"token"`
			User   struct {
				Login string `json:"login"`
				Email string `json:"email"`
			} `json:"user"`
			Key struct {
				Name string `json:"name"`
				// Sent by the install since the flow was built, and read here
				// for the first time.
				ExpiresAt time.Time `json:"expires_at"`
			} `json:"key"`
		}
		err := c.Do(http.MethodPost, "/api/v1/auth/cli/device/token",
			map[string]string{"device_code": deviceCode}, &answer)

		var apiErr *client.Error
		if errors.As(err, &apiErr) && apiErr.Code == "slow_down" {
			wait += time.Second
			continue
		}
		if err != nil {
			return approval{}, err
		}

		switch answer.Status {
		case "pending":
			continue
		case "denied":
			return approval{}, fmt.Errorf("that sign-in was refused in the browser")
		case "expired":
			return approval{}, fmt.Errorf("the code expired before it was approved — run `toft login` again")
		case "approved":
			who := answer.User.Email
			if who == "" {
				who = answer.User.Login
			}
			return approval{
				Token:     answer.Token,
				User:      who,
				KeyName:   answer.Key.Name,
				ExpiresAt: answer.Key.ExpiresAt,
			}, nil
		default:
			return approval{}, fmt.Errorf("the install answered with a status this client does not know (%q) — upgrade toft", answer.Status)
		}
	}
}

// loginWithToken is the CI machine's path: a key a human minted in a browser,
// arriving on stdin so it never reaches an argument list.
func loginWithToken(e *env, installURL string) error {
	line, err := readLineCtx(e.ctx, os.Stdin)
	if errors.Is(err, context.Canceled) {
		return err
	}
	if err != nil && line == "" {
		return usageCodef("empty_input", "no token on stdin — try `toft login --token-stdin < token.txt`")
	}
	token := strings.TrimSpace(line)
	if token == "" {
		return usageCodef("empty_input", "no token on stdin")
	}
	if !strings.HasPrefix(token, "toft") {
		// Refused before the first request, as a TOFT_TOKEN of the same shape
		// is: sent, it reached the install (BB-054, fifth 2026-11-03 list).
		err := notAToftKey("the key on stdin").(*authError)
		err.hint = "pipe the whole key the install issued: `toft login --token-stdin " + installURL + " < key.txt`"
		return err
	}

	// Before the key is sent: the address has to be an install, not an app
	// (BB-26).
	if err := checkIsInstall(installURL); err != nil {
		return err
	}

	// Verified before it is written, so a mistyped paste fails here rather than
	// on the next deploy with a message about that deploy.
	c := client.New(installURL, token, "toft/"+version)
	who, err := verifyToken(c, token)
	if err != nil {
		return err
	}

	expires := keyExpiry(c)

	creds, err := config.LoadCredentials()
	if err != nil {
		return err
	}
	setKey(creds, installURL, token, who, expires)
	if err := saveCreds(e, creds); err != nil {
		return err
	}
	if config.IsDeployKeyToken(token) {
		if e.json {
			return signedInDocument(e, installURL, "", "", true, expires)
		}
		// Not "Signed in as": there is nobody to name. A deploy key is the one
		// credential class with no person behind it, and saying so here is the
		// same distinction `toft status --mine` already enforces.
		e.out.Say("%s Stored a deploy key for %s — it belongs to nobody and owns nothing",
			e.out.OK("✓"), installURL)
		return nil
	}
	if e.json {
		return signedInDocument(e, installURL, who, "", false, expires)
	}
	if e.out.Rich() {
		e.out.Say("%s Signed in to %s as %s", e.out.OK("✓"), e.out.Link(installURL, installURL), e.out.Bold(who))
		return nil
	}
	e.out.Say("%s Signed in to %s as %s", e.out.OK("✓"), installURL, who)
	return nil
}

// setKey replaces an install's key and keeps what else the entry holds — its
// alias and a stored session. Re-keying is not signing out of the browser half.
func setKey(creds *config.CredentialsFile, installURL, token, who string, expires time.Time) {
	c, found, _ := creds.Lookup(installURL)
	if !found {
		c = config.Credential{}
	}
	c.URL, c.Token, c.User, c.ExpiresAt = installURL, token, who, expires
	creds.Set(c)
}

// loginWithPassword is the unattended administrator's sign-in: the password
// form a browser posts, from a program.
//
// **It stores a session, and a session is not a key.** The key a machine
// already holds is left as it was; the session goes beside it and is presented
// only by a command run with `--session`. That is the whole of the design: a
// key is refused every route that decides who may do what, on purpose
// (internal/api/keyroutes.go), and nothing here changes that — a session is
// what an administrator's browser holds, it lives the install's session
// lifetime rather than ninety days, it is revoked by `Sign out everywhere`, and
// it exists only where somebody holding the password asked for it.
//
// Only the built-in administrator signs in this way: SSO has no password to
// read, and the install answers 404 when local sign-in is switched off.
func loginWithPassword(e *env, installURL, user string) error {
	line, err := readLineCtx(e.ctx, os.Stdin)
	if errors.Is(err, context.Canceled) {
		return err
	}
	if err != nil && line == "" {
		return usageCodef("empty_input", "no password on stdin — try `printf %%s \"$PASSWORD\" | toft login --password-stdin %s`", installURL)
	}
	password := strings.TrimRight(line, "\r\n")
	if password == "" {
		return usageCodef("empty_input", "no password on stdin")
	}
	// Before the password is sent: the address has to be an install (BB-26).
	if err := checkIsInstall(installURL); err != nil {
		return err
	}

	c := client.New(installURL, "", "toft/"+version)
	body, _ := json.Marshal(map[string]string{"username": user, "password": password})
	ctx := e.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		installURL+"/api/v1/auth/local/login", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	// A body carrying the password is never re-sent: a 307 or 308 would have
	// the client POST it again to wherever the Location names. A redirect is
	// the answer, and a refusal.
	hc := *c.HTTP
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return &client.UnreachableError{URL: installURL, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return authf("%s answered the sign-in with a redirect to %q, and the password was not sent on. "+
			"If that address is this install's, log in at it", installURL, resp.Header.Get("Location"))
	}
	var answer struct {
		OK                 bool   `json:"ok"`
		MustChangePassword bool   `json:"must_change_password"`
		Reason             string `json:"reason"`
		Error              string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&answer)

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return &authError{code: "local_sign_in_off", hint: "sign in with `toft login " + installURL + "` instead, or ask an administrator to turn local sign-in on",
			msg: fmt.Sprintf("%s does not take a password: the built-in administrator's sign-in is "+
				"switched off. An administrator turns it on under Admin → Authentication", installURL)}
	case resp.StatusCode == http.StatusTooManyRequests:
		return &authError{code: "rate_limited", hint: "wait a few minutes, then sign in again",
			msg: fmt.Sprintf("%s is refusing sign-ins from here for now (too many attempts) — wait, then retry", installURL)}
	case resp.StatusCode != http.StatusOK:
		msg := answer.Error
		if msg == "" {
			msg = resp.Status
		}
		// Coded and hinted, as every refusal is (L5, 2026-11-04 black-box list).
		return &authError{code: "invalid_credentials",
			hint: fmt.Sprintf("check the password for %s (and --user, if it is not admin), then run the same command again", user),
			msg:  fmt.Sprintf("%s refused the password for %s: %s", installURL, user, msg)}
	case answer.MustChangePassword:
		return authf("%s: %s Sign in once in a browser to choose one, then retry", installURL, answer.Reason)
	}

	var token string
	var expires time.Time
	for _, ck := range resp.Cookies() {
		if ck.Name == client.SessionCookie && ck.Value != "" {
			token = ck.Value
			if ck.MaxAge > 0 {
				expires = time.Now().Add(time.Duration(ck.MaxAge) * time.Second).UTC()
			}
		}
	}
	if token == "" {
		return fmt.Errorf("%s accepted the password and set no %s cookie — an install older than this client?",
			installURL, client.SessionCookie)
	}

	// Asked of the install, as the session, before anything is written — the
	// same rule as a pasted key.
	verify := client.New(installURL, token, "toft/"+version)
	verify.Cookie = true
	who, err := verifyToken(verify, token)
	if err != nil {
		return err
	}

	creds, err := config.LoadCredentials()
	if err != nil {
		return err
	}
	entry, found, _ := creds.Lookup(installURL)
	if !found {
		entry = config.Credential{}
	}
	entry.URL, entry.Session, entry.SessionUser, entry.SessionExpiresAt = installURL, token, who, expires
	creds.Set(entry)
	if err := saveCreds(e, creds); err != nil {
		return err
	}
	if e.json {
		return json.NewEncoder(e.out.Stdout).Encode(map[string]any{
			"install": installURL, "user": who, "session": true, "expires_at": expires,
		})
	}
	e.out.Say("%s Signed in to %s as %s — a session, until %s", e.out.OK("✓"), installURL, who,
		expires.Local().Format("15:04 MST 2 Jan"))
	e.out.Dim("  Commands use it only with --session; the key stays the default.")
	return nil
}

// keyExpiry is when the install says this credential stops working. A pasted
// key carries no expiry the client can read, so without asking, `toft login
// --status` said "expires unknown" for every --token-stdin sign-in. Zero when
// the key never expires or the install is older than the field; both are
// "unknown" to the status line, which is the truth in the second case.
func keyExpiry(c *client.Client) time.Time {
	var caps struct {
		Credential struct {
			ExpiresAt *time.Time `json:"expires_at"`
		} `json:"credential"`
	}
	if err := c.Do(http.MethodGet, "/api/v1/me/capabilities", nil, &caps); err != nil || caps.Credential.ExpiresAt == nil {
		return time.Time{}
	}
	return caps.Credential.ExpiresAt.UTC()
}

// verifyToken asks the install a question this credential's class is allowed to
// answer, and reports who it belongs to — empty for a deploy key, which belongs
// to nobody.
//
// The class matters because `/auth/me` is mounted for personal keys only
// (`internal/api/router.go`, `With(s.apiKeyAuth)`), and the deploy key's
// allow-list contains no /auth route at all. Probing it with an `toftd_` key
// therefore reports a perfectly valid credential as invalid, and sends whoever
// hit it to rotate a key that did not need rotating.
//
// The prefix selects the probe and nothing else. The install still
// authenticates against a stored hash, so a token that lies about its class
// fails the probe it selected — which is what keeps this a hint for better copy
// rather than a check, as DeployKeyPrefix's comment requires.
func verifyToken(c *client.Client, token string) (string, error) {
	if config.IsDeployKeyToken(token) {
		// On the deploy key's allow-list, and a route it has to reach to be
		// useful at all. Nothing in the answer is wanted: that it answered is
		// the whole of the check.
		if err := c.Do(http.MethodGet, "/api/v1/apps", nil, nil); err != nil {
			return "", err
		}
		return "", nil
	}
	var me struct {
		Login string `json:"login"`
		Email string `json:"email"`
	}
	if err := c.Do(http.MethodGet, "/api/v1/auth/me", nil, &me); err != nil {
		return "", err
	}
	// An answer that names nobody is not the install's: `/auth/me` always says
	// who a personal credential belongs to, and an app behind the gateway
	// answering 200 {} printed "Signed in … as " and stored the key (BB-26).
	if me.Email == "" && me.Login == "" {
		return "", usageHinted("not_an_install",
			"sign in to the install's own address — the one you open Toft at in a browser",
			"%s answered the sign-in check without naming anyone, which an install never does — "+
				"nothing was stored", c.BaseURL)
	}
	if me.Email != "" {
		return me.Email, nil
	}
	return me.Login, nil
}

// loginStatus answers the question somebody actually has, which is not "what is
// in the file" but **"what happens if I run `toft deploy` here"**.
//
// So it runs the real resolution, manifests included, from the current
// directory, and leads with the answer. The old note — that TOFT_TOKEN
// "beats everything below" — is no longer true in general now that a token has
// to name its install, and a precedence note was the wrong answer to the
// question anyway.
//
// When resolution refuses, it prints the refusal and exits 0: being told why is
// this command working, not failing.
func loginStatus(e *env) error {
	resolver, err := config.NewResolver(e.dir, nil)
	if err != nil {
		return err
	}
	sess, resolveErr := config.ResolveSession(resolver.Install())
	switch {
	case resolveErr != nil:
		e.out.Warn("%v", resolveErr)
	default:
		who := sess.User
		if sess.IsDeployKey() {
			who = "(deploy key)"
		} else if who == "" {
			who = "(unknown)"
		}
		e.out.Answer("%s  %s", sess.InstallURL, who)
		e.out.Dim("  from %s", sess.Origin)
		switch {
		case sess.ExpiresAt.IsZero():
			// Never-written and expired stay tellable apart: an entry with no
			// expiry is unknown, which is not never.
			e.out.Dim("  expires unknown")
		default:
			e.out.Dim("  expires %s", sess.ExpiresAt.UTC().Format("2006-01-02"))
		}
	}
	e.out.Say("")

	creds, err := config.LoadCredentials()
	if err != nil {
		return err
	}
	if len(creds.Installs) == 0 {
		if e.out.Rich() {
			e.out.Say("Not signed in anywhere.")
			e.out.Next("toft login <url>")
			return nil
		}
		e.out.Say("Not signed in anywhere. Run `toft login <url>`.")
		return nil
	}
	if e.out.Rich() {
		printCredentialsRich(e, creds)
		return nil
	}
	for url, c := range creds.Installs {
		marker := "  "
		if config.NormalizeInstallURL(creds.Default) == url {
			marker = e.out.OK("* ")
		}
		who := c.User
		switch {
		case config.IsDeployKeyToken(c.Token):
			// The class answers this, not the stored field: a deploy key has no
			// person behind it, and an empty `user` is also what an old entry
			// written before the class was recognised looks like.
			who = e.out.Dimmed("(deploy key)")
		case who == "":
			who = e.out.Dimmed("(unknown)")
		}
		alias := ""
		if c.Alias != "" {
			alias = e.out.Dimmed("  (" + c.Alias + ")")
		}
		expiry := ""
		if !c.ExpiresAt.IsZero() {
			expiry = e.out.Dimmed("  expires " + c.ExpiresAt.UTC().Format("2006-01-02"))
		}
		e.out.Answer("%s%s  %s%s%s", marker, url, who, alias, expiry)
		if c.Session != "" {
			state := "until " + c.SessionExpiresAt.Local().Format("15:04 MST 2 Jan")
			if !c.SessionExpiresAt.IsZero() && !c.SessionExpiresAt.After(time.Now()) {
				state = "expired"
			}
			e.out.Dim("    + session as %s, %s (used with --session)", c.SessionUser, state)
		}
	}
	e.out.Dim("Stored in %s", config.CredentialsPath())
	return nil
}

// loginStatusJSON is `--status` for a program: one object, not the two-part
// display.
//
// **A resolution failure is a field, not an error.** "Which install would this
// directory talk to, and as whom" is the question, and *nothing, because the
// stored default and the environment disagree* is one of its answers. Exiting
// non-zero here would make the command that exists to diagnose a broken
// credential indistinguishable from one that hit a broken credential.
//
// No token, in any field, ever. This is the one command whose whole subject is
// a secret, and its output is the thing most likely to be pasted somewhere.
func loginStatusJSON(e *env) error {
	doc := map[string]any{}

	resolver, err := config.NewResolver(e.dir, nil)
	if err != nil {
		return err
	}
	if sess, resolveErr := config.ResolveSession(resolver.Install()); resolveErr != nil {
		doc["current"] = nil
		doc["problem"] = resolveErr.Error()
	} else {
		current := map[string]any{
			"install":    sess.InstallURL,
			"origin":     sess.Origin,
			"deploy_key": sess.IsDeployKey(),
		}
		if sess.User != "" {
			current["user"] = sess.User
		}
		// Absent rather than a zero time: an entry with no expiry recorded is
		// *unknown*, which is not the same as never and must not serialise as a
		// date in year one.
		if !sess.ExpiresAt.IsZero() {
			current["expires_at"] = sess.ExpiresAt.UTC()
		}
		doc["current"] = current
	}

	creds, err := config.LoadCredentials()
	if err != nil {
		return err
	}
	installs := make([]map[string]any, 0, len(creds.Installs))
	for url, c := range creds.Installs {
		entry := map[string]any{
			"install":    url,
			"default":    config.NormalizeInstallURL(creds.Default) == url,
			"deploy_key": config.IsDeployKeyToken(c.Token),
		}
		if c.User != "" {
			entry["user"] = c.User
		}
		if c.Alias != "" {
			entry["alias"] = c.Alias
		}
		if !c.ExpiresAt.IsZero() {
			entry["expires_at"] = c.ExpiresAt.UTC()
		}
		if c.Session != "" {
			// Who and until when; never the session itself.
			entry["session"] = map[string]any{"user": c.SessionUser, "expires_at": c.SessionExpiresAt.UTC()}
		}
		installs = append(installs, entry)
	}
	sort.Slice(installs, func(i, j int) bool {
		return installs[i]["install"].(string) < installs[j]["install"].(string)
	})
	doc["installs"] = installs
	doc["credentials_path"] = config.CredentialsPath()

	return json.NewEncoder(e.out.Stdout).Encode(doc)
}

func loginForget(e *env, which string, all bool) error {
	creds, err := config.LoadCredentials()
	if err != nil {
		return err
	}
	if all {
		urls := make([]string, 0, len(creds.Installs))
		for u := range creds.Installs {
			urls = append(urls, u)
		}
		sort.Strings(urls)
		creds.ForgetAll()
		if err := saveCreds(e, creds); err != nil {
			return err
		}
		// Under --json the answer is a document like every other command's
		// (BB-016, third 2026-11-03 list): the installs whose credential went.
		if e.json {
			return json.NewEncoder(e.out.Stdout).Encode(map[string]any{"forgotten": urls})
		}
		e.out.Say("Forgot every stored credential.")
		// Said because it is true and easy to assume otherwise: this file is
		// the only thing that was dropped. The keys themselves still exist on
		// the installs and still work; revoking them is a different act, in a
		// different place, and pretending otherwise would be a security claim
		// this command cannot keep.
		e.out.Dim("The keys still exist on those installs — revoke them under Account → API keys.")
		return nil
	}
	forgotten, err := creds.Forget(which)
	if err != nil {
		return err
	}
	if !forgotten {
		return usageCodef("no_credential", "no stored credential for %q", which)
	}
	if err := saveCreds(e, creds); err != nil {
		return err
	}
	if e.json {
		return json.NewEncoder(e.out.Stdout).Encode(map[string]any{"forgotten": []string{which}})
	}
	e.out.Say("Forgot the credential for %s.", which)
	e.out.Dim("The key still exists on that install — revoke it under Account → API keys.")
	return nil
}

// browsable reports whether the install's verification address may be handed
// to the system's opener: an http(s) URL on the install's own host. The opener
// is xdg-open, open or rundll32, and each runs whatever a scheme is registered
// to — a `file:` desktop entry, a UNC path to an executable — so an address the
// install chose is opened only when it is a page of that install, and printed
// otherwise.
func browsable(target, installURL string) bool {
	t, err := url.Parse(target)
	if err != nil || (t.Scheme != "https" && t.Scheme != "http") || t.Host == "" {
		return false
	}
	i, err := url.Parse(installURL)
	return err == nil && strings.EqualFold(t.Hostname(), i.Hostname())
}

// openBrowser tries to open a URL, and reports whether it got as far as
// starting something. It never waits: the browser is a convenience and the
// printed URL is the contract.
func openBrowser(url string) bool {
	if url == "" || os.Getenv("DISPLAY") == "" && runtime.GOOS == "linux" {
		// No display: an ssh session or a container, which is exactly the case
		// the device flow exists for. Print the URL and say nothing about it.
		return false
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start() == nil
}

// skipHelp turns `--help` into a clean exit.
func skipHelp(err error) error {
	if errors.Is(err, error(errHelp)) {
		return nil
	}
	return err
}

// printCredentialsRich is the stored credentials drawn for a person: one row
// per install, the default marked, sorted so the order is the same twice, and
// an expiry coloured by how close it is.
func printCredentialsRich(e *env, creds *config.CredentialsFile) {
	urls := make([]string, 0, len(creds.Installs))
	for url := range creds.Installs {
		urls = append(urls, url)
	}
	sort.Strings(urls)
	t := e.out.NewTable("", "install", "as", "expires").Flex(1)
	var sessions []string
	for _, url := range urls {
		c := creds.Installs[url]
		marker := " "
		if config.NormalizeInstallURL(creds.Default) == url {
			marker = e.out.OK("●")
		}
		who := c.User
		switch {
		case config.IsDeployKeyToken(c.Token):
			who = e.out.Dimmed("deploy key")
		case who == "":
			who = e.out.Dimmed("unknown")
		}
		if c.Alias != "" {
			who += e.out.Dimmed("  (" + c.Alias + ")")
		}
		expiry := e.out.Dimmed("—")
		if !c.ExpiresAt.IsZero() {
			expiry = c.ExpiresAt.UTC().Format("2006-01-02")
			switch left := time.Until(c.ExpiresAt); {
			case left <= 0:
				expiry = e.out.Fail(expiry + " expired")
			case left <= config.ExpiryWarningWindow:
				expiry = e.out.Caution(expiry + " (" + untilWords(c.ExpiresAt) + ")")
			}
		}
		t.Row(marker, e.out.Link(url, url), who, expiry)
		if c.Session != "" {
			state := "until " + c.SessionExpiresAt.Local().Format("15:04 MST 2 Jan")
			if !c.SessionExpiresAt.IsZero() && !c.SessionExpiresAt.After(time.Now()) {
				state = "expired"
			}
			sessions = append(sessions, fmt.Sprintf("%s: + session as %s, %s (used with --session)", url, c.SessionUser, state))
		}
	}
	t.Print()
	for _, line := range sessions {
		e.out.Dim("  %s", line)
	}
	e.out.Dim("● is the default. Stored in %s", config.CredentialsPath())
}

// untilWords is how long until t, in the words a person uses: "in 6 days".
func untilWords(t time.Time) string {
	d := time.Until(t)
	switch {
	case d <= 0:
		return "expired"
	case d < time.Hour:
		return "in under an hour"
	case d < 48*time.Hour:
		return fmt.Sprintf("in %d hours", int(d.Hours()))
	}
	return fmt.Sprintf("in %d days", int(d.Hours()/24))
}
