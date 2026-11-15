package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// isolateConfigDir points the config and credential files at a temporary
// directory, so a test never reads or writes the developer's own.
func isolateConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	// The Windows branch reads this one; setting both keeps the test honest on
	// either platform.
	t.Setenv("AppData", dir)
	return dir
}

// The file holds a credential, so its permissions are part of the design and
// not an implementation detail.
func TestCredentialsAreWrittenPrivate(t *testing.T) {
	isolateConfigDir(t)

	f := &CredentialsFile{Installs: map[string]Credential{}}
	f.Set(Credential{URL: "https://toft.corp", Token: "toftu_secret", User: "ada@corp.com"})
	if err := f.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(CredentialsPath())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("mode = %o, want 600 — this file holds a token", perm)
	}

	// And it round-trips, including the default, which is what `toft deploy` with
	// no --install reads.
	read, err := LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if read.Default != "https://toft.corp" {
		t.Errorf("default = %q, want the only install", read.Default)
	}
	c, ok, err := read.Lookup("https://toft.corp")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !ok || c.Token != "toftu_secret" {
		t.Errorf("lookup = %+v, %v", c, ok)
	}
}

// An alias and a URL are the same lookup, so `--install staging` and
// `--install https://…` cannot resolve to two different installs.
func TestAliasAndURLAreOneLookup(t *testing.T) {
	isolateConfigDir(t)
	f := &CredentialsFile{Installs: map[string]Credential{}}
	f.Set(Credential{URL: "https://staging.corp", Alias: "staging", Token: "toftu_s"})
	f.Set(Credential{URL: "https://toft.corp", Alias: "prod", Token: "toftu_p"})

	byAlias, ok, _ := f.Lookup("staging")
	if !ok {
		t.Fatal("alias did not resolve")
	}
	byURL, ok, _ := f.Lookup("https://staging.corp/")
	if !ok {
		t.Fatal("URL with a trailing slash did not resolve")
	}
	if byAlias.Token != byURL.Token {
		t.Errorf("alias and URL resolved to different installs (%q vs %q)", byAlias.URL, byURL.URL)
	}
}

// A default pointing at an install somebody has just signed out of is a command
// that fails for a reason nobody can see.
func TestForgetMovesTheDefault(t *testing.T) {
	isolateConfigDir(t)
	f := &CredentialsFile{Installs: map[string]Credential{}}
	f.Set(Credential{URL: "https://a.corp", Token: "toftu_a"})
	f.Set(Credential{URL: "https://b.corp", Token: "toftu_b"})
	f.Default = "https://a.corp"

	forgotten, err := f.Forget("https://a.corp")
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if !forgotten {
		t.Fatal("Forget said there was nothing to forget")
	}
	if f.Default != "https://b.corp" {
		t.Errorf("default = %q, want it moved to the surviving install", f.Default)
	}
	if _, ok, _ := f.Lookup("https://a.corp"); ok {
		t.Error("the forgotten credential is still there")
	}
}

// With two or more installs left, nobody picks: a default chosen from a map
// sent the next deploy to staging or prod at random (code review 2026-11-11,
// M-2). The person names one, or ResolveSession asks.
func TestForgetAmongManyLeavesNoDefault(t *testing.T) {
	isolateConfigDir(t)
	f := &CredentialsFile{Installs: map[string]Credential{}}
	f.Set(Credential{URL: "https://a.corp", Token: "toftu_a"})
	f.Set(Credential{URL: "https://b.corp", Token: "toftu_b"})
	f.Set(Credential{URL: "https://c.corp", Token: "toftu_c"})
	f.Default = "https://a.corp"
	if _, err := f.Forget("https://a.corp"); err != nil {
		t.Fatal(err)
	}
	if f.Default != "" {
		t.Errorf("default = %q, want none: two installs remain and neither was chosen", f.Default)
	}
}

// The CI path: an install named in the environment brings its own token, and
// neither half falls back to a developer's stored file.
//
// The install has to come from TOFT_URL for the token to be used at all —
// see TestEnvTokenNeedsEnvInstall — so what this pins is the other half: when
// the pair *is* complete, the environment still beats a stored credential for
// the same install. That is the documented precedence, and the job that set both
// said which one it meant.
func TestEnvironmentTokenBeatsTheFile(t *testing.T) {
	isolateConfigDir(t)
	f := &CredentialsFile{Installs: map[string]Credential{}}
	f.Set(Credential{URL: "https://toft.corp", Token: "toftu_from_file", User: "ada@corp.com"})
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOFT_TOKEN", "toftu_from_env")

	s, err := ResolveSession(Value{"https://toft.corp", envVar[KeyInstall]})
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	if s.Token != "toftu_from_env" {
		t.Errorf("token = %q, want the environment's", s.Token)
	}
}

// An install nobody has signed in to is a different fix from no install at all,
// and both messages name the command that works.
func TestRefusalsNameTheFix(t *testing.T) {
	isolateConfigDir(t)
	t.Setenv("TOFT_TOKEN", "")

	if _, err := ResolveSession(Value{}); err != ErrNoInstall {
		t.Errorf("with nothing configured: %v, want ErrNoInstall", err)
	}

	_, err := ResolveSession(Value{"https://toft.corp", "--install"})
	var notSignedIn *NotSignedInError
	if err == nil || !asNotSignedIn(err, &notSignedIn) {
		t.Fatalf("with an unknown install: %v, want NotSignedInError", err)
	}
	if !strings.Contains(err.Error(), "toft login") {
		t.Errorf("the refusal does not name the command that works: %v", err)
	}
}

func asNotSignedIn(err error, target **NotSignedInError) bool {
	e, ok := err.(*NotSignedInError)
	if ok {
		*target = e
	}
	return ok
}

// **A credential and an address are one statement or they are nothing.**
//
// This is the test whose failure would be silent. If the pairing rule regresses,
// a production token in somebody's shell is sent to whatever install resolved —
// from a flag, from a toft.toml somebody else committed, from the stored
// default — the request succeeds or fails on the far end's terms, and every
// observable output is identical either way. By the time anyone could notice,
// the secret has been transmitted.
func TestEnvTokenNeedsEnvInstall(t *testing.T) {
	// Each case names the install from a different layer. None of them is the
	// environment, so none of them completes the pair.
	refused := []struct {
		name    string
		install Value
	}{
		{"a flag", Value{"https://staging.corp", "--install"}},
		{"a committed manifest", Value{"https://staging.corp", ManifestFile + " [app] install"}},
		{"an uncommitted manifest", Value{"https://staging.corp", LocalManifestFile + " [app] install"}},
		{"the stored default", Value{}},
	}

	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfigDir(t)
			f := &CredentialsFile{Installs: map[string]Credential{}}
			f.Set(Credential{URL: "https://staging.corp", Token: "toftu_stored"})
			if err := f.Save(); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TOFT_TOKEN", "toftu_production_secret")

			s, err := ResolveSession(tc.install)
			if err == nil {
				t.Fatalf("resolved to %q — an unbound token was pointed at an install "+
					"the environment never named", s.Token)
			}
			var unbound *AmbiguousCredentialError
			if !errors.As(err, &unbound) {
				t.Fatalf("err = %v, want AmbiguousCredentialError", err)
			}
			// The refusal is only useful if it names both halves and the
			// one-line fix in both directions.
			for _, want := range []string{"TOFT_TOKEN", "TOFT_URL", "https://staging.corp"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
		})
	}

	// With TOFT_URL set too, the environment did name the token's install, and
	// the conflict is the flag. Telling the user to set TOFT_URL — already
	// set, to that value — sent them round in a circle (the trytoft drills).
	t.Run("the pair and a flag", func(t *testing.T) {
		isolateConfigDir(t)
		t.Setenv("TOFT_TOKEN", "toftu_from_env")
		t.Setenv("TOFT_URL", "https://staging.corp")

		_, err := ResolveSession(Value{"https://prod.corp", "--install"})
		var unbound *AmbiguousCredentialError
		if !errors.As(err, &unbound) {
			t.Fatalf("err = %v, want AmbiguousCredentialError", err)
		}
		if strings.Contains(err.Error(), "set TOFT_URL=") {
			t.Errorf("the refusal asks for the variable that is already set: %v", err)
		}
		if !strings.Contains(err.Error(), "drop --install") {
			t.Errorf("the refusal does not name the flag as the conflict: %v", err)
		}

		// A manifest is not dropped; the install it names is (review #9).
		_, err = ResolveSession(Value{"https://prod.corp", "toft.toml"})
		if err == nil || strings.Contains(err.Error(), "drop toft.toml") ||
			!strings.Contains(err.Error(), "remove the install toft.toml names") {
			t.Errorf("a manifest's conflict = %v, want it to say to remove the install it names", err)
		}
	})

	// The same install named twice is one statement, not a conflict.
	t.Run("the pair and a flag that agrees", func(t *testing.T) {
		isolateConfigDir(t)
		t.Setenv("TOFT_TOKEN", "toftu_from_env")
		t.Setenv("TOFT_URL", "https://staging.corp")

		s, err := ResolveSession(Value{"https://staging.corp/", "--install"})
		if err != nil {
			t.Fatalf("--install naming TOFT_URL's install = %v, want it to proceed", err)
		}
		if s.Token != "toftu_from_env" {
			t.Errorf("token = %q, want the environment's", s.Token)
		}
	})

	// And the pair itself proceeds. A rule that refused this would have broken
	// the pipeline case it exists to serve.
	t.Run("the pair proceeds", func(t *testing.T) {
		isolateConfigDir(t)
		t.Setenv("TOFT_TOKEN", "toftu_from_env")

		s, err := ResolveSession(Value{"https://toft.corp", envVar[KeyInstall]})
		if err != nil {
			t.Fatalf("ResolveSession: %v", err)
		}
		if s.Token != "toftu_from_env" || s.InstallURL != "https://toft.corp" {
			t.Errorf("resolved %s with %q, want the environment's pair", s.InstallURL, s.Token)
		}
	})
}

// **An alias that names two installs is refused, not resolved.**
//
// The second silent one. `Lookup` used to return the first alias match it found
// while ranging a map, so the same alias resolved to either install depending on
// what the runtime yielded — and what that produces is not an error anybody
// sees, it is a deploy that *works*, against the wrong install.
func TestAmbiguousAliasIsRefused(t *testing.T) {
	isolateConfigDir(t)

	f := &CredentialsFile{Installs: map[string]Credential{}}
	f.Set(Credential{URL: "https://one.corp", Token: "toftu_one", Alias: "prod"})
	f.Set(Credential{URL: "https://two.corp", Token: "toftu_two", Alias: "prod"})

	// Refused on read, for a file that already contains the collision.
	_, ok, err := f.Lookup("prod")
	if ok {
		t.Fatal("an ambiguous alias resolved — that is a deploy to the wrong install")
	}
	var ambiguous *AmbiguousAliasError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("err = %v, want AmbiguousAliasError", err)
	}
	for _, want := range []string{"prod", "https://one.corp", "https://two.corp"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}

	// And refused on write, so the state is unreachable going forward.
	if err := f.Save(); err == nil {
		t.Error("Save wrote a file in which one alias names two installs")
	}

	// The unambiguous alias beside it still resolves — the refusal is about the
	// collision and not about aliases.
	f.Installs["https://two.corp"] = Credential{URL: "https://two.corp", Token: "toftu_two", Alias: "staging"}
	c, ok, err := f.Lookup("prod")
	if err != nil || !ok || c.URL != "https://one.corp" {
		t.Errorf("Lookup(prod) = %q, %v, %v — want the one install that holds it", c.URL, ok, err)
	}
	if err := f.Save(); err != nil {
		t.Errorf("Save refused a file with two distinct aliases: %v", err)
	}
}

// **Two writers, and both entries survive.**
//
// The third silent one, and the one whose failure is most delayed: Save was a
// whole-file read-modify-write, so two `toft login` runs in parallel each
// serialised the set they had read and the second rename won. The loser did not
// find out at the time — they found out days later, in a different session,
// being asked to sign in to an install they had signed in to.
//
// The lock is what fixes it, and re-reading inside the lock is what makes the
// lock worth having: a writer that applied its mutation to what it read before
// the wait would take the lock and still lose the entry.
// A save writes back only what this process changed: an entry it merely
// loaded is the disk's, so a key another process stored for the same install
// in between survives (security review 2026-11-15, CL-7).
func TestASaveKeepsAnEntryItDidNotChange(t *testing.T) {
	isolateConfigDir(t)
	seed := &CredentialsFile{Installs: map[string]Credential{}}
	seed.Set(Credential{URL: "https://x.corp", Token: "toftu_old"})
	if err := seed.Save(); err != nil {
		t.Fatal(err)
	}

	a, err := LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	b.Set(Credential{URL: "https://x.corp", Token: "toftu_new"})
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	a.Set(Credential{URL: "https://y.corp", Token: "toftu_y"})
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}

	final, err := LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if x, _, _ := final.Lookup("https://x.corp"); x.Token != "toftu_new" {
		t.Errorf("x = %q, want the key the other process stored", x.Token)
	}
	if _, ok, _ := final.Lookup("https://y.corp"); !ok {
		t.Error("y, this process's own change, is missing")
	}
}

func TestConcurrentSavesKeepBothCredentials(t *testing.T) {
	isolateConfigDir(t)

	const writers = 8
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each writer loads and saves independently, which is what two
			// processes do — the sharing is the file, not the struct.
			f, err := LoadCredentials()
			if err != nil {
				errs <- err
				return
			}
			f.Set(Credential{
				URL:   fmt.Sprintf("https://install-%d.corp", i),
				Token: fmt.Sprintf("toftu_%d", i),
			})
			if err := f.Save(); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Save: %v", err)
	}

	final, err := LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	for i := 0; i < writers; i++ {
		url := fmt.Sprintf("https://install-%d.corp", i)
		if _, ok, _ := final.Lookup(url); !ok {
			t.Errorf("%s is missing — a concurrent Save lost it", url)
		}
	}

	// And nothing is left lying around beside it. A temporary file holding a
	// token is the same secret as the file it was going to become.
	entries, err := os.ReadDir(filepath.Dir(CredentialsPath()))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") || strings.HasSuffix(e.Name(), ".lock") {
			t.Errorf("%s was left behind", e.Name())
		}
	}
}

// A merge that was a plain union would make --forget a no-op the moment anything
// else had written the file. It is the one case where "keep what is on disk" is
// the wrong answer, so it is tested next to the case that motivates the merge.
func TestForgettingSurvivesTheMerge(t *testing.T) {
	isolateConfigDir(t)

	f := &CredentialsFile{Installs: map[string]Credential{}}
	f.Set(Credential{URL: "https://a.corp", Token: "toftu_a"})
	f.Set(Credential{URL: "https://b.corp", Token: "toftu_b"})
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}

	forgetter, err := LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := forgetter.Forget("https://a.corp"); err != nil {
		t.Fatal(err)
	}
	if err := forgetter.Save(); err != nil {
		t.Fatal(err)
	}

	final, err := LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := final.Lookup("https://a.corp"); ok {
		t.Error("the merge put back a credential that was forgotten")
	}
	if _, ok, _ := final.Lookup("https://b.corp"); !ok {
		t.Error("the merge dropped a credential that was not forgotten")
	}
}

// A credentials file nobody can write is worse than a race nobody hit.
func TestStaleLockIsBroken(t *testing.T) {
	isolateConfigDir(t)

	// A lock from an `toft` that was killed: present, and older than any real
	// write could be.
	path := CredentialsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	lockPath := path + ".lock"
	if err := os.WriteFile(lockPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * lockStale)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatal(err)
	}

	f := &CredentialsFile{Installs: map[string]Credential{}}
	f.Set(Credential{URL: "https://toft.corp", Token: "toftu_secret"})

	done := make(chan error, 1)
	go func() { done <- f.Save() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Save wedged on a dead process's lock: %v", err)
		}
	case <-time.After(lockWait + 2*time.Second):
		t.Fatal("Save never returned — a killed toft locked somebody out of their own credentials")
	}
	if !f.BrokeStaleLock {
		t.Error("the stale break was silent; stepping over another lock should be said")
	}

	// A *live* lock is not broken, and the wait is bounded rather than forever.
	if err := os.WriteFile(lockPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(lockPath)
	start := time.Now()
	if err := f.Save(); err == nil {
		t.Error("Save stepped over a lock that was taken moments ago")
	}
	if waited := time.Since(start); waited > lockWait+2*time.Second {
		t.Errorf("waited %s on a held lock, want a bounded wait", waited)
	}
}

// **The secret arrives by path, and the path is not the secret.**
//
// An environment variable is readable through /proc, inherited by every child
// and printed by every diagnostic that dumps the environment. A mounted file is
// how every secret store in a container hands one over.
func TestTokenFileIsTheOtherDoor(t *testing.T) {
	isolateConfigDir(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	// Written the way a secret store writes one: with a trailing newline,
	// trimmed exactly as --token-stdin trims.
	if err := os.WriteFile(path, []byte("toftd_from_a_mount\n"), 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TOFT_TOKEN", "")
	t.Setenv("TOFT_TOKEN_FILE", path)

	s, err := ResolveSession(Value{"https://toft.corp", envVar[KeyInstall]})
	if err != nil {
		t.Fatalf("ResolveSession: %v", err)
	}
	if s.Token != "toftd_from_a_mount" {
		t.Errorf("token = %q, want the file's contents trimmed", s.Token)
	}

	// It is the same half of the same pair, arriving by a different door: an
	// install this machine did not get from the environment still refuses.
	if _, err := ResolveSession(Value{"https://staging.corp", "--install"}); err == nil {
		t.Error("a file-borne token was used against an install the environment never named")
	} else if !strings.Contains(err.Error(), "TOFT_TOKEN_FILE") {
		t.Errorf("the refusal names the wrong door: %v", err)
	}
}

// Two tokens, one run. Not a precedence — a job that exports both does not know
// which one it used, and choosing for it would make that true silently.
func TestTokenFileAndTokenTogetherRefuse(t *testing.T) {
	isolateConfigDir(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("toftd_from_a_mount"), 0600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		token string
		file  string
		want  string
	}{
		{"both set", "toftu_from_env", path, "both set"},
		// A volume that did not arrive and a store that handed over nothing are
		// different mistakes, and get different sentences.
		{"the file is not there", "", filepath.Join(dir, "absent"), "was not mounted"},
		{"the file is empty", "", filepath.Join(dir, "empty"), "is empty"},
	}
	if err := os.WriteFile(filepath.Join(dir, "empty"), []byte("  \n"), 0600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TOFT_TOKEN", tc.token)
			t.Setenv("TOFT_TOKEN_FILE", tc.file)

			_, err := ResolveSession(Value{"https://toft.corp", envVar[KeyInstall]})
			var envErr *EnvCredentialError
			if !errors.As(err, &envErr) {
				t.Fatalf("err = %v, want EnvCredentialError", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to say %q", err, tc.want)
			}
		})
	}
}

// **Fourteen days, because the unit that has to see this is a run, not a day.**
//
// A weekly pipeline gets two chances at fourteen days and one at seven, and one
// chance is a coin flip against a job that was skipped that week.
//
// The boundary that matters more than either number is the top one: an expired
// credential produces no warning at all. The CLI never decides a credential has
// expired — the install answers 401 key_expired and that is exit 3 — so a client
// that warned here would be second-guessing the only party that knows.
func TestExpiryWarningBoundary(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		expires time.Time
		warn    bool
	}{
		{"fifteen days is silent", now.Add(15 * 24 * time.Hour), false},
		{"fourteen days warns", now.Add(14*24*time.Hour - time.Minute), true},
		{"tomorrow warns", now.Add(24 * time.Hour), true},
		{"already expired does not warn — the install refuses, and says so", now.Add(-time.Hour), false},
		{"unknown is not never", time.Time{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{InstallURL: "https://toft.corp", ExpiresAt: tc.expires}
			got := s.ExpiryWarning(now)
			if tc.warn && got == "" {
				t.Fatal("no warning, and the first sign will be a pipeline's exit 3")
			}
			if !tc.warn && got != "" {
				t.Fatalf("warned when it should not have: %q", got)
			}
			if tc.warn && !strings.Contains(got, "toft login") {
				t.Errorf("the warning does not name the fix: %q", got)
			}
		})
	}
}

// The warning names the key's door as well as the browser's: `toft login
// <url>` alone is the device flow, which waits for a person, and the holder of
// a key is as often a CI runner (docs/bugs 2026-10-28 D20). A deploy key has
// no browser door at all.
func TestExpiryWarningNamesTheKeyDoor(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	personal := &Session{InstallURL: "https://toft.corp", Token: "toftu_x", ExpiresAt: now.Add(48 * time.Hour)}
	got := personal.ExpiryWarning(now)
	if !strings.Contains(got, "toft login --token-stdin https://toft.corp") || !strings.Contains(got, "toft login https://toft.corp") {
		t.Errorf("a personal credential's warning should name both doors: %q", got)
	}
	deploy := &Session{InstallURL: "https://toft.corp", Token: DeployKeyPrefix + "x", ExpiresAt: now.Add(48 * time.Hour)}
	got = deploy.ExpiryWarning(now)
	if !strings.Contains(got, "--token-stdin") || strings.Contains(got, "toft login https://toft.corp`") {
		t.Errorf("a deploy key's warning should name the key door alone: %q", got)
	}
}

// The expiry is stored because the server already sends it, and an entry written
// before the field existed has to stay tellable from one that never expires.
// Every string comes back as it was written, whatever characters it holds —
// Go's %q spelled BEL and VT in escapes TOML does not have, and the file was
// then unreadable for every install (security review 2026-11-15, CL-5).
func TestCredentialStringsRoundTrip(t *testing.T) {
	isolateConfigDir(t)
	user := "a\ab\vc\"d\\e\x1b\x7f"
	alias := "tab\there"
	f := &CredentialsFile{Installs: map[string]Credential{}}
	f.Set(Credential{URL: "https://odd.corp", Token: "toftu_a", User: user, Alias: alias,
		Session: "jwt\a", SessionUser: "admin\v"})
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	read, err := LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials after a save: %v", err)
	}
	c, ok, _ := read.Lookup("https://odd.corp")
	if !ok || c.User != user || c.Alias != alias || c.Session != "jwt\a" || c.SessionUser != "admin\v" {
		t.Errorf("read back %+v", c)
	}
	// An invalid byte is replaced, not turned into another character.
	if got := tomlString("x\xffy"); got != "\"x\uFFFDy\"" {
		t.Errorf("tomlString of an invalid byte = %q", got)
	}
}

func TestExpiryRoundTrips(t *testing.T) {
	isolateConfigDir(t)
	expires := time.Date(2026, 11, 19, 9, 30, 0, 0, time.UTC)

	f := &CredentialsFile{Installs: map[string]Credential{}}
	f.Set(Credential{URL: "https://known.corp", Token: "toftu_a", ExpiresAt: expires})
	f.Set(Credential{URL: "https://unknown.corp", Token: "toftu_b"})
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}

	read, err := LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	known, _, _ := read.Lookup("https://known.corp")
	if !known.ExpiresAt.Equal(expires) {
		t.Errorf("expires_at = %v, want %v", known.ExpiresAt, expires)
	}
	unknown, _, _ := read.Lookup("https://unknown.corp")
	if !unknown.ExpiresAt.IsZero() {
		t.Errorf("expires_at = %v, want the zero value — unknown is not a date", unknown.ExpiresAt)
	}
}
