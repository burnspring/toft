package api

import (
	"context"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"toft/internal/builder"
	"toft/internal/database"
	mailer "toft/internal/mail"
	"toft/internal/notify"
)

// An app on a hostname of its own: assigned by an administrator, served at /,
// with /apps/{slug} redirecting to it once its release has moved.

// notifyHostnameChange tells an app's owners that an administrator switched
// its hostname on or off. Sent at the setter, not at the flip: the owner's
// links are about to move whether or not the rebuild succeeds, and its
// failure is already a deploy failure with a notice of its own.
func (s *Server) notifyHostnameChange(app *database.App, on bool) {
	if s.mailer == nil {
		return
	}
	gateway := builder.PublicURL(s.cfg.AppsBaseURL, &database.App{Slug: app.Slug})
	host := s.cfg.AppHostOrigin(app.Hostname) + "/"
	data := notify.TemplateData{AppName: app.Name, Address: host, PreviousAddress: gateway}
	if !on {
		data.Address, data.PreviousAddress = gateway, host
	}
	people, err := s.db.ListAppDeciders(app.ID)
	if err != nil || len(people) == 0 {
		if err != nil {
			slog.Error("hostname notify: listing the app's owners", "app_id", app.ID, "error", err)
		}
		return
	}
	data.BaseURL = s.mailer.BaseURL()
	msg, err := mailer.Render("app_hostname_changed", data)
	if err != nil {
		slog.Error("hostname notify: rendering", "error", err)
		return
	}
	for _, p := range people {
		msg.To = append(msg.To, p.Address)
	}
	s.mailer.SendAsync(msg)
}

// appHostKey carries the host a request was resolved to an app by, on an app
// host; absent on every other request. urlPrefixKey carries the prefix the
// request arrived on, for the handlers past the guard.
type appHostKey struct{}
type urlPrefixKey struct{}

// onAppHost reports whether this request arrived on an app's own hostname.
func onAppHost(r *http.Request) bool {
	_, ok := r.Context().Value(appHostKey{}).(string)
	return ok
}

// requestedApp is the app a gateway request is for, and the prefix it arrived
// on: by hostname on an app host, at "" — every path there is that app's — and
// by slug at /apps/{slug} everywhere else. Each call is one indexed read, the
// same the slug lookup always was.
func (s *Server) requestedApp(r *http.Request) (*database.App, string, error) {
	if host, ok := r.Context().Value(appHostKey{}).(string); ok {
		app, err := s.db.GetAppByServedHostname(host)
		return app, "", err
	}
	slug := chi.URLParam(r, "slug")
	app, err := s.db.GetAppBySlug(slug)
	return app, builder.BasePath(slug), err
}

// gatewayOrigin is the origin this gateway request is served on: the app's
// own on an app host, the apps origin otherwise. A sign-in returns here.
func (s *Server) gatewayOrigin(r *http.Request) string {
	if host, ok := r.Context().Value(appHostKey{}).(string); ok {
		return s.cfg.AppHostOrigin(host)
	}
	return s.cfg.AppsBaseURL
}

// appHostURL is an address on an app's own hostname: the path remainder below
// its old prefix, and the query, kept.
func (s *Server) appHostURL(app *database.App, rest, rawQuery string) string {
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	u := s.cfg.AppHostOrigin(app.Hostname) + rest
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return u
}

// appHostOf is the host a request names, when it could be an app's: what a
// trusted proxy declared, else the Host header, port dropped and lowercased.
// "" for the install's own hosts and for anything ParseAppHostname would
// never have stored — a single label, an IP literal — so the common request
// costs no read at all.
func (s *Server) appHostOf(r *http.Request) string {
	host := requestHost(r)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || !strings.Contains(host, ".") || net.ParseIP(host) != nil {
		return ""
	}
	for _, h := range s.cfg.ReservedHosts() {
		if strings.EqualFold(h, host) {
			return ""
		}
	}
	return host
}

// appHostDispatch resolves an app host before anything else on the router it
// wraps. On a match, two reserved paths are served as on the gateway — the
// handshake's redeem and security.txt — and **every other path is that
// app's**: /apps/<another> on reports.example.com is reports' own path, because
// anything else would put a neighbour on this origin and make the isolation a
// name. No match falls through to the router unchanged.
//
// Mounted on both routers: the apps router after its logging, realIP,
// telemetry and HSTS and before the apps origin check, and the single-origin
// platform router before every route, so on that install an app host never
// sees the SPA or /api/v1.
func (s *Server) appHostDispatch(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No app has a hostname: nothing to resolve, and no read — the install
		// that never uses the feature pays nothing for it on any request.
		host := ""
		if s.appHostnamesRecorded() {
			host = s.appHostOf(r)
		}
		if host == "" {
			next.ServeHTTP(w, r)
			return
		}
		app, err := s.db.GetAppByServedHostname(host)
		if err != nil {
			log.Printf("proxy: resolving host %s: %v", host, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if app == nil {
			next.ServeHTTP(w, r)
			return
		}
		r = r.WithContext(withAppHost(r, host))
		var h http.Handler
		switch r.URL.Path {
		case appHandshakePath:
			h = http.HandlerFunc(s.handleAppSessionRedeem)
		case securityTxtPath:
			h = http.HandlerFunc(s.handleSecurityTxt)
		default:
			h = s.guardedAppProxy(http.HandlerFunc(s.handleAppProxy), s.gatewayUnresolved)
		}
		s.appHostOriginCheck(h).ServeHTTP(w, r)
	})
}

// withAppHost is a request's context as appHostDispatch leaves it.
func withAppHost(r *http.Request, host string) context.Context {
	return context.WithValue(r.Context(), appHostKey{}, host)
}
