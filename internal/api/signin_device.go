package api

import (
	"net/http"
	"sync"
	"time"

	"toft/internal/auth"
)

// The browser half of local sign-in's lockout — see auth/signindevice.go for
// why the thing counted is a browser rather than an address.

const (
	// signinDeviceCookie is set only after a correct password, and sent only
	// to the two local sign-in routes.
	signinDeviceCookie     = auth.CookiePrefix + "signin_device"
	signinDeviceCookiePath = "/api/v1/auth/local"

	// deviceMaxFailures and deviceLockout are the account's own numbers,
	// applied to one browser: a remembered browser mistyping is held to the
	// rule everyone else is, it just cannot be held to it by somebody else.
	deviceMaxFailures = 10
	deviceLockout     = 15 * time.Minute
)

// signinDevice is the id of the remembered browser this request comes from for
// userID and the password set at passwordSetAt, or "".
func (s *Server) signinDevice(r *http.Request, userID string, passwordSetAt time.Time) string {
	c, err := r.Cookie(signinDeviceCookie)
	if err != nil || c.Value == "" {
		return ""
	}
	return s.jwt.VerifySigninDevice(c.Value, userID, passwordSetAt, time.Now())
}

// rememberSigninDevice marks this browser as one that has signed in as userID
// with the password set at passwordSetAt. A browser that already holds a live
// token for that password keeps it, so its counter survives.
func (s *Server) rememberSigninDevice(w http.ResponseWriter, r *http.Request, userID string, passwordSetAt time.Time) {
	if s.signinDevice(r, userID, passwordSetAt) != "" {
		return
	}
	tok := s.jwt.MintSigninDevice(userID, passwordSetAt, time.Now())
	if tok == "" {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     signinDeviceCookie,
		Value:    tok,
		Path:     signinDeviceCookiePath,
		HttpOnly: true,
		Secure:   s.secureCookies(),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(auth.SigninDeviceTTL.Seconds()),
	})
}

// deviceThrottle counts failures per remembered browser. Entries exist only for
// device ids that verified, so the map is bounded by real browsers, not by
// whoever is guessing.
type deviceThrottle struct {
	mu sync.Mutex
	m  map[string]*deviceFailures
}

type deviceFailures struct {
	count       int
	lockedUntil time.Time
}

// locked reports whether the device is refusing attempts, and until when.
func (d *deviceThrottle) locked(id string, now time.Time) (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f := d.m[id]
	if f == nil || !f.lockedUntil.After(now) {
		return time.Time{}, false
	}
	return f.lockedUntil, true
}

// fail counts one wrong password. Like the account's counter it is not reset by
// the lockout lapsing, only by a correct password — so waiting out one lockout
// buys one attempt, not ten.
func (d *deviceThrottle) fail(id string, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.m == nil {
		d.m = map[string]*deviceFailures{}
	}
	f := d.m[id]
	if f == nil {
		f = &deviceFailures{}
		d.m[id] = f
	}
	f.count++
	if f.count >= deviceMaxFailures {
		f.lockedUntil = now.Add(deviceLockout)
	}
}

func (d *deviceThrottle) clear(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.m, id)
}
