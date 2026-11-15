package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/gorilla/websocket"

	"log/slog"
	"toft/internal/advisory"
	"toft/internal/appint"
	"toft/internal/applog"
	"toft/internal/artifact"
	"toft/internal/auth"
	"toft/internal/builder"
	"toft/internal/hoststore"

	"toft/internal/config"
	"toft/internal/crypto"
	"toft/internal/database"
	"toft/internal/deps"
	"toft/internal/depsweep"
	"toft/internal/entitlements"
	"toft/internal/events"
	"toft/internal/gallery"
	"toft/internal/gitconn"
	"toft/internal/health"
	"toft/internal/idle"
	"toft/internal/jobs"
	"toft/internal/presence"
	"toft/internal/resource"
	"toft/internal/runtime"
	"toft/internal/supervisor"
	"toft/internal/telemetry"
	"toft/internal/usage"
)

type Server struct {
	// pool is the Database pool check's memory of recent waits (WatchPool).
	pool poolWatch

	// hostnamesSeen caches whether any app has a hostname recorded, which
	// decides the platform's session cookie (hostSessionCookie).
	hostnamesSeen atomic.Pointer[hostnamesCache]

	// repoPreflight asks the remote whether a repository and branch exist,
	// before POST /apps claims a slug for them. Nil skips it (tests, and any
	// wiring that has no git), which is the old behaviour.
	repoPreflight func(ctx context.Context, repoURL, branch, token string) error
	// subdirPreflight asks whether a folder exists on that branch; nil skips
	// the question. Only an answer of "no" refuses.
	subdirPreflight func(ctx context.Context, repoURL, branch, token, subdir string) (bool, error)

	cfg *config.Config
	db  *database.DB
	jwt *auth.JWTService
	// rts holds one execution backend per mode. Handlers that act on an app's
	// live workload — logs, metrics, stop, restart, delete — go through
	// runtimeFor(app), never through a single backend: an isolated app's
	// workload is a pod, and asking the process manager about it returns
	// "not found" rather than an answer.
	rts       *runtime.Runtimes
	builder   *builder.Builder
	encryptor *crypto.Encryptor
	usage     *usage.Recorder
	// presence answers "who is online now" from memory. Unlike usage, nothing
	// about it is persisted: it is a statement about the last few minutes on
	// this node, which is why it needs no store and survives no restart.
	presence *presence.Tracker
	// idle is the activity the reaper reads to decide what to sleep — the same
	// tracker instance, since a reaper reading a different one would see an
	// estate nobody had ever visited. Memory only, for the same reason as
	// presence.
	idle *idle.Tracker
	// supervisor is the isolated backend's crash-recovery loop. The proxy asks
	// it one question — is this app still being restarted — because the crashed
	// interstitial promises an automatic refresh and that promise is a lie once
	// it has stopped trying. Nil on an install with no isolated backend, and a
	// nil one answers yes, which is the copy this page has always shown.
	supervisor *supervisor.Supervisor
	// accounts removes a deleted app's OS account; nil where apps get none.
	accounts AppAccounts
	// appOutput is the shared apps' output copy, nil where there is none.
	appOutput AppOutput
	// health is the crash sweep, asked the second question the crashed
	// interstitial needs: an app the platform stopped for repeatedly running
	// out of memory is not being restarted by anything, in either mode, so the
	// supervisor's answer — which is yes for every shared app — is not the
	// whole of it. Nil on an install wired without one, and a nil one answers
	// no.
	health *health.Watcher
	// logLevel is how much this process says, settable for its lifetime. Nil on
	// a Server wired without one; every write through it is nil-safe.
	logLevel *slog.LevelVar
	// platformLog is what this platform said about each app, and the live
	// fan-out of what it is saying now. Nil on an install wired without one,
	// and every read through it then answers as it did before.
	platformLog *applog.Sink
	// wakes collapses the burst of requests a sleeping app receives into one
	// wake, so a link to a slept app restarts it once rather than once per
	// asset on the page. It is also what the Restart button goes through, so
	// two people pressing it are one start.
	wakes *idle.WakeGroup
	// wakeHold is how long a caller waits on a start before being told to come
	// back — the proxy's first visitor and the Restart button share it, because
	// it answers one question and should not be able to answer it twice.
	wakeHold time.Duration
	// forgiven, when set, is told every app a restart forgave — the tests'
	// window onto D10's rule that a scheduled restart never does. Nil in
	// production.
	forgiven func(appID string)
	// artifacts is nil when the install offers no artifact store. Isolated
	// workloads cannot exist without one, so the runner callbacks refuse
	// rather than degrade.
	artifacts artifact.Store
	// consumption is the same recorder the shared collector feeds, so a
	// self-reporting workload and a sampled one land in one metric model.
	consumption *resource.Recorder
	// jobs is nil when the install offers no isolated backend. The endpoints
	// still exist and still answer — with the explanation of why jobs are
	// unavailable, which is more useful than a 404 on a route the UI knows
	// about.
	jobs *jobs.Service
	// storage binds durable resources to workloads. Nil when the install offers
	// no isolated backend, and the storage routes then 404 — absent
	// configuration means the capability is not offered, not that it exists and
	// fails.
	storage runtime.StorageProvisioner
	// storeSigner mints presigned URLs into a store's own bucket, for a person
	// holding a grant rather than an app holding a mount. Nil unless the
	// operator switched uploads on — which is also the moment they granted the
	// platform its first data-plane permission on their buckets — and the
	// /api/v1/store routes then 404. Absent, not dormant.
	//
	// The interface, not a concrete backend: this package signs into somebody
	// else's bucket and has no business knowing whose cloud it is. A typed nil
	// pointer stored here would read as present and fail at the first call, so
	// the caller's variable is declared as the interface too.
	storeSigner artifact.Signer
	// hostStore is the data plane of a resource under a host root: URLs the
	// platform signs for itself and serves at /store-data/. Nil where it is
	// not offered, and hostStoreAbsent then says why for Admin → Checks.
	hostStore       *hoststore.Store
	hostStoreAbsent string
	// mailer is nil when the install was started without email support, and
	// reports itself disabled when no relay is configured. Both cases mean the
	// same thing to a notification: it is not sent, nothing errors, and the
	// thing that would have been announced is still in the UI to be found.
	mailer Mailer
	// slack is nil when the install was started without Slack support, and
	// reports itself disabled when no bot token is configured. Both cases mean
	// the same thing to a notification: it is not posted, nothing errors, and
	// the thing that would have been announced is still in the UI to be found.
	slack Slacker
	// posters is every other channel a platform notification is posted to —
	// the catalog poster, which serves every non-Slack kind (a Teams bot
	// carrying the admin audience, say). Never holds Slack: postPlatform asks
	// s.slack itself, and holding it here too would post twice.
	posters    []events.Poster
	spaHandler http.Handler
	upgrader   websocket.Upgrader
	// appSender is the bounded queue behind app-initiated Slack and email. Nil
	// when neither integration is available to this build, in which case the
	// /api/v1/app/notify routes refuse rather than pretend.
	appSender *appint.Sender
	// transports is the catalog's registry: one entry per kind, built once at
	// startup beside the queue. A kind in appint.Known with no entry here is a
	// send that can never go out, which is why a test walks both lists.
	transports transportRegistry
	// appTokenLimiters and appTokenTouched are per-app-token state on the
	// request path: a rate bucket and a last-used throttle. In memory and
	// node-local, like every other counter here — an app's notification must
	// not add a database write per call.
	appTokenLimiters sync.Map
	appTokenTouched  sync.Map
	// appTokenSwept is when the two maps above were last swept, in Unix
	// nanoseconds (sweepAppTokenState).
	appTokenSwept atomic.Int64
	// signinDevices is the per-browser failure count for local sign-in (see
	// signin_device.go). In memory: the platform runs one replica, and a
	// restart forgetting it costs a browser at most ten more guesses.
	signinDevices deviceThrottle
	// apiKeyTouched throttles last_used_at writes per key (see touchUserAPIKey).
	// Shared by both key classes — the id spaces are UUIDs and the
	// budget being enforced is the same one.
	apiKeyTouched sync.Map
	// connectTasks follows the deploys the Connect surface started, so a
	// polling client sees their build log as it is written (connect_handlers.go).
	connectTasks connectTaskLog
	// gitConn is the one reader of a clone credential, shared with the builder
	// and the auto-deploy poller. The API layer holds it to drop a
	// connection's cached client and minted token on every write to the row,
	// which is what makes a disable take effect on the next mint rather than
	// on the next restart. Built here when absent, so a Server constructed in
	// a test does not panic on a nil pointer.
	gitConn *gitconn.Resolver
	// autodeploy is the poller, so a verified webhook delivery can shorten the
	// wait for a push. Nil on an install wired without one, and a nil one means
	// a verified delivery does nothing — the same outcome as no delivery at
	// all, which is the whole promise of the webhook being an accelerator.
	autodeploy AutodeployNudger
	// gallery is the cached catalog. Nil in tests and on any build that was
	// started without it, in which case the gallery routes 404 — the same
	// answer an install gets when an administrator has switched it off, and
	// for the same reason: absent means not offered.
	gallery *gallery.Service
	// mux is the platform router, kept so apiKeyRouteGuard can resolve which
	// route a request will land on *before* it lands there. Middleware runs
	// above the inner routing, so chi has not filled the pattern in yet.
	mux *chi.Mux
	// muxRoutes is every "METHOD pattern" the platform router serves, walked
	// once when mux is assigned. chi's Find resolves a registered path under a
	// method no handler serves — PUT /api/v1/apps came back as the GET's
	// pattern — so a match is only a route when this set has the pair too.
	muxRoutes map[string]bool
	// ents answers gate 1 — has this install bought the feature. Nil means the
	// permissive default, which is every install until a licensing system
	// exists; read through s.entitlements() so the default lives in one place.
	ents entitlements.Source

	// deps resolves the dependency policy — the settings and the entitlement
	// together (ADR §39). Nil is safe: Policy() answers "off", so every
	// dependency surface reports the feature absent rather than panicking.
	deps *deps.Resolver
	// advisories is the corpus syncer, for the administrator's Sync now
	// button. Nil means no button — the same install shape that has no
	// outbound request at all.
	advisories *advisory.Service
	// depSweeper re-derives findings on demand, for Check again. Nil means the
	// route reports the feature absent.
	depSweeper *depsweep.Sweeper
	// announcer sends the announcement an administrator wrote, and owns the
	// goroutine that does it. Nil on a Server built without one, in which case
	// every surface reports the install as unable to send — the same answer an
	// install with no relay gives, which is the honest one.
	announcer Announcer
}

// WithAnnouncer supplies the announcement send runner. Absent, composing still
// works and sending does not appear: absent means not offered, and a Send
// button that promises mail an install cannot send is "copy may not promise
// mail" broken by a control rather than by a sentence.
func WithAnnouncer(a Announcer) ServerOption {
	return func(s *Server) { s.announcer = a }
}

func NewServer(cfg *config.Config, db *database.DB, jwt *auth.JWTService, rts *runtime.Runtimes, b *builder.Builder, enc *crypto.Encryptor, opts ...ServerOption) *Server {
	s := &Server{
		cfg:       cfg,
		db:        db,
		jwt:       jwt,
		rts:       rts,
		builder:   b,
		encryptor: enc,
		usage:     usage.NewRecorder(db),
		presence:  presence.New(),
		wakes:     idle.NewWakeGroup(),
		wakeHold:  wakeHoldTimeout,
	}
	for _, opt := range opts {
		opt(s)
	}
	// Tests construct a Server without a recorder; a nil one would panic on
	// the first consumption report rather than on the line that forgot it.
	if s.consumption == nil {
		s.consumption = resource.NewRecorder(nopConsumptionStore{})
	}
	// A Server with no tracker is one nothing reaps — a nil *idle.Tracker is
	// inert by construction, so the proxy needs no branch for it. Left nil
	// rather than defaulted: a tracker the reaper does not share would record
	// activity nobody ever reads.
	// The credential resolver. Supplied by main so the builder, the poller and
	// this layer share one cache; built here otherwise, because a Server that
	// panicked on a route nobody wired would fail in the wrong place.
	if s.gitConn == nil {
		s.gitConn = gitconn.NewResolver(db, enc)
	}
	s.upgrader = websocket.Upgrader{CheckOrigin: s.checkWebSocketOrigin}
	// The app-integration queue. Guarded on db because tests construct a
	// Server without one, and a startup sweep against a nil database would
	// panic here rather than at the line that forgot it.
	if db != nil {
		s.startAppIntegrations()
	}
	// toft.toml's [[storage]] is judged here, with the console's validator
	// and the delegations, and the builder asks through this interface.
	if b != nil && db != nil {
		b.SetStoragePlanner(s)
	}
	return s
}

// ServerOption supplies a dependency that only some installs have. Variadic
// rather than more positional parameters: the artifact store and the
// consumption recorder are both optional, and a constructor with two trailing
// nils at most call sites invites passing them in the wrong order.
type ServerOption func(*Server)

// WithArtifacts supplies the release store. Absent, isolated workloads are
// refused at bootstrap rather than started and left unable to fetch anything.
func WithArtifacts(store artifact.Store) ServerOption {
	return func(s *Server) { s.artifacts = store }
}

// AppAccounts is the host side of an app's own OS account: its name, and its
// removal when the app is deleted (runtime.Isolation).
type AppAccounts interface {
	AccountName(slug string) string
	RemoveAccount(slug string) error
}

// AppOutput is the slice of the output copy (internal/appout) an app's page
// reports: whether the cap applies and what it refused.
type AppOutput interface {
	Uncapped() bool
	DroppedSinceStart(appID string) int64
	// Line copies one line, as the tailer's are: a host job run's, here.
	Line(runtime.OutputLine)
}

// WithAppOutput supplies it. Absent, no app's policy block has a log_copy.
func WithAppOutput(o AppOutput) ServerOption {
	return func(s *Server) { s.appOutput = o }
}

// WithAppAccounts supplies it. Absent, a delete leaves accounts as it found them.
func WithAppAccounts(a AppAccounts) ServerOption {
	return func(s *Server) { s.accounts = a }
}

// WithJobs supplies the job service. Absent, the job endpoints answer with
// the reason jobs are unavailable rather than disappearing.
func WithJobs(j *jobs.Service) ServerOption {
	return func(s *Server) { s.jobs = j }
}

// WithIdle supplies the activity tracker the idle reaper reads. It must be the
// same *idle.Tracker the reaper was built with: the proxy is the only thing
// that knows an app was used, and the reaper is the only thing that acts on it.
// Absent, nothing records activity and nothing sleeps.
func WithIdle(t *idle.Tracker) ServerOption {
	return func(s *Server) { s.idle = t }
}

// WithWakeGroup shares the gateway's WakeGroup with the idle reaper, whose
// end-of-window queue wakes isolated apps through it: one group is what makes
// a visitor and the queue reaching the same app share one start.
func WithWakeGroup(g *idle.WakeGroup) ServerOption {
	return func(s *Server) { s.wakes = g }
}

// WithSupervisor supplies the crash-recovery loop, so the crashed interstitial
// can stop promising a refresh once it has given up and a deliberate restart
// can clear what it has counted. Absent, the page reads as it always did.
func WithSupervisor(sup *supervisor.Supervisor) ServerOption {
	return func(s *Server) { s.supervisor = sup }
}

// WithHealth supplies the crash sweep, so the crashed interstitial can say when
// an app was stopped for running out of memory rather than promise a restart
// nothing is performing, and a deliberate restart can clear what it counted.
func WithHealth(w *health.Watcher) ServerOption {
	return func(s *Server) { s.health = w }
}

// WithPlatformLog supplies the sink holding the platform's own account of each
// app. It must be the same *applog.Sink the writers were built with: the tail
// reads what it stored and the live socket subscribes to what it is about to
// store, and two sinks would make a pane that showed neither.
//
// Absent, the routes answer exactly what they answered before — the backend's
// tail alone. Absent, not off.
func WithPlatformLog(sink *applog.Sink) ServerOption {
	return func(s *Server) { s.platformLog = sink }
}

// WithLogLevel supplies the level obs.Init installed, so a settings write can
// change how much this process says without restarting it.
//
// The one dial that takes effect immediately, and the reason it may: it reaches
// no workload at all. Absent — a test — the setting is stored and read like any
// other and this process's own verbosity does not move, which is the honest
// behaviour for a Server nobody wired a logger into.
// WithRepoPreflight supplies the remote check POST /apps makes before it
// writes anything — `git ls-remote` in production.
func WithRepoPreflight(fn func(ctx context.Context, repoURL, branch, token string) error) ServerOption {
	return func(s *Server) { s.repoPreflight = fn }
}

// WithSubdirPreflight supplies the folder half of that check — a blob-less
// one-commit clone in production (builder.SubdirExists).
func WithSubdirPreflight(fn func(ctx context.Context, repoURL, branch, token, subdir string) (bool, error)) ServerOption {
	return func(s *Server) { s.subdirPreflight = fn }
}

func WithLogLevel(lv *slog.LevelVar) ServerOption {
	return func(s *Server) { s.logLevel = lv }
}

// WithConsumption supplies the recorder the shared collector already feeds, so
// self-reported and sampled figures share one metric model.
func WithConsumption(r *resource.Recorder) ServerOption {
	return func(s *Server) { s.consumption = r }
}

// WithGitConnections supplies the credential resolver the builder and the
// poller share. Absent, the Server builds its own — which is correct in a test
// and wrong in production, where a second cache would mean an administrator
// disabling a connection took effect on two of three readers.
func WithGitConnections(r *gitconn.Resolver) ServerOption {
	return func(s *Server) { s.gitConn = r }
}

// WithGallery supplies the catalog service. Absent, every gallery route 404s
// and nothing about the gallery appears in the UI.
func WithGallery(g *gallery.Service) ServerOption {
	return func(s *Server) { s.gallery = g }
}

// WithEntitlements supplies the license source. Absent, the install is
// permissively licensed and says so — see internal/entitlements for why that is
// the honest default rather than a hole.
func WithEntitlements(e entitlements.Source) ServerOption {
	return func(s *Server) { s.ents = e }
}

// WithStorage supplies the backend that binds durable resources to workloads.
// Absent, the storage routes 404 and nothing about storage appears in the UI.
func WithStorage(p runtime.StorageProvisioner) ServerOption {
	return func(s *Server) { s.storage = p }
}

// WithStoreSigner supplies the signer behind `toft store put` — presigned upload
// and download against a store, for a person holding a grant.
//
// Separate from WithStorage because they are two different capabilities that an
// install can have independently: binding a store to an app needs no AWS
// permission at all, and signing into one needs PutObject/GetObject/ListBucket
// on its prefix. Absent, the /api/v1/store routes 404 and nothing about uploads
// appears in the UI.
func WithStoreSigner(signer artifact.Signer) ServerOption {
	return func(s *Server) { s.storeSigner = signer }
}

// WithHostStore supplies the host data plane, or nil with the reason it is
// absent — no root, apps sharing an account, or a kernel that lets an app
// hard-link a file it cannot read.
func WithHostStore(store *hoststore.Store, absent string) ServerOption {
	return func(s *Server) { s.hostStore, s.hostStoreAbsent = store, absent }
}

// WithDependencies supplies the three halves of dependency policy: the policy
// resolver the builder also holds, the corpus syncer behind Sync now, and the
// sweeper behind Check again.
//
// One option rather than three because they are one capability, and an install
// with the resolver and no syncer would offer a page whose refresh button did
// nothing. Absent, every dependency route reports the feature absent — the
// same answer an administrator switching it off gets.
func WithDependencies(r *deps.Resolver, feed *advisory.Service, sweeper *depsweep.Sweeper) ServerOption {
	return func(s *Server) {
		s.deps, s.advisories, s.depSweeper = r, feed, sweeper
	}
}

// runtimeFor returns the backend that owns this app's live workload.
//
// It reads the *running* mode, not the desired one. Asking for the desired mode
// would send every promoted-but-not-yet-redeployed app to a backend that has
// never heard of it, and the caller would render a running app as missing.
func (s *Server) runtimeFor(app *database.App) (runtime.ServiceRuntime, error) {
	return s.rts.For(app.RunningModeOrShared())
}

// nopConsumptionStore discards intervals. Used only when no recorder was
// supplied, which is a test.
type nopConsumptionStore struct{}

func (nopConsumptionStore) RecordConsumption([]resource.Interval) error { return nil }

// FlushUsage drains buffered usage counters; called on graceful shutdown.
func (s *Server) FlushUsage() { s.usage.Flush() }

func (s *Server) Router(spaHandler http.Handler) http.Handler {
	s.spaHandler = spaHandler
	r := chi.NewRouter()

	// One structured line per request, naming the caller — see accesslog.go for
	// why this can be outermost and still know who that was, which chi's
	// middleware.Logger could not.
	r.Use(accessLog(telemetry.SurfacePlatform))
	r.Use(middleware.Recoverer)
	// Forwarded-for headers are honored only from configured proxies; chi's
	// RealIP believed them from anyone, letting callers choose their own
	// audit-log IP.
	if len(s.cfg.TrustedProxyCIDRs) > 0 {
		r.Use(realIPMiddleware(s.cfg.TrustedProxyCIDRs))
	}
	// Telemetry, after realIP and before everything else: it needs the
	// trusted-proxy verdict that realIP records, and the duration it measures
	// should include the origin check rather than start after it. Installed
	// always, because telemetry can be switched on from Admin → Telemetry
	// after boot; with no pipeline live it costs one atomic load per request.
	r.Use(telemetry.Middleware(telemetry.SurfacePlatform, s.inboundTraceTrusted))
	// The request id, after realIP because it needs that trusted-proxy verdict,
	// and before the origin check so a refused origin carries one too. It is what a person quotes when they report an
	// error and what an operator matches an audit row against.
	r.Use(requestIDMiddleware)
	// What a browser needs in order to defend these pages — CSP, the
	// anti-clickjacking pair, nosniff, a referrer policy. Outside the /api/v1
	// subtree because the SPA is the surface that needs them most, and skipping
	// deployed apps from the inside: see security_headers.go for why an app's
	// pages are not the platform's to constrain.
	r.Use(s.securityHeaders)
	// HSTS, beside them and never taken off an app's response: it is about the
	// host, and the host is the install's (security_headers.go).
	r.Use(s.hsts(s.cfg.BaseURL))

	// Dev password protection — gates all routes behind HTTP Basic Auth
	if s.cfg.DevPassword != "" {
		r.Use(s.devBasicAuth)
	}
	// An app on a hostname of its own, resolved before every route: on a
	// single-origin install this listener is the gateway too, and an app host
	// must never see the SPA, /api/v1 or the platform's origin check — every
	// path there is the app's (app_hostname.go).
	r.Use(s.appHostDispatch)
	// Posit Connect clients address /__api__; this moves them onto
	// /api/v1/connect before anything routes, so every layer below — CORS,
	// origin check, both key authenticators, both route guards — treats them
	// as the platform API they are. After appHostDispatch, because on an app
	// host /__api__ is the app's own path like every other (app_hostname.go),
	// and after the access log, which records the path the client sent.
	r.Use(s.connectAPIRewrite)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: []string{s.cfg.BaseURL, "http://localhost:5173"},
		// PUT belongs here: grants, the announcement, and app tags are all
		// PUT endpoints. Its absence was invisible in dev only because the
		// Vite proxy makes those calls same-origin.
		AllowedMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{"Accept", "Content-Type"},
		// Without this the SPA cannot read the request id off a failed response
		// cross-origin, which is the one thing it would want it for. A UUID this
		// process generated discloses nothing.
		ExposedHeaders:   []string{"X-Request-ID"},
		AllowCredentials: true,
	}))

	// CORS only decides which responses a browser will hand to script; it does
	// not stop the request from executing. This does — the platform's rule
	// everywhere, except the inline app proxy of a single-origin install,
	// which takes the apps gateway's (rootOriginCheck).
	r.Use(s.rootOriginCheck)

	// Public client configuration (no auth needed)
	r.Get("/api/v1/config", s.handlePublicConfig)

	// The one static file this binary answers for by hand, and the reason is
	// `/` now rendering a page to anyone who can reach it.
	r.Get("/robots.txt", s.handleRobots)

	// Where to send what you found, on the origin Toft itself is served
	// from. The apps gateway mounts the same handler — see AppsRouter — and on
	// a single-origin install this is the only listener there is. 404 until an
	// administrator has filled in a contact.
	r.Get(securityTxtPath, s.handleSecurityTxt)

	// The SCIM feed (ADR §48). Outside /api/v1 deliberately: RFC 7644 specifies
	// these path shapes and an IdP builds them from a base URL it is given, so
	// the version here is the protocol's rather than this product's.
	//
	// Nothing about the platform API's authentication reaches this subtree — no
	// cookie session, no API key, no deploy key. A directory's token is the only
	// credential these routes accept, and `TestEveryRouteIsClassified` does not
	// cover anything outside /api/v1, so `TestEverySCIMRouteIsGuarded` is what
	// stands in for it.
	r.Route("/scim/v2", func(r chi.Router) {
		r.Use(requestMetaMiddleware)
		r.Use(s.scimAuth)

		r.Get("/ServiceProviderConfig", s.handleSCIMServiceProviderConfig)
		r.Get("/ResourceTypes", s.handleSCIMResourceTypes)
		r.Get("/Schemas", s.handleSCIMSchemas)

		r.Get("/Users", s.handleSCIMListUsers)
		r.Get("/Users/{id}", s.handleSCIMGetUser)
		r.Patch("/Users/{id}", s.handleSCIMPatchUser)
		r.Post("/Users", s.handleSCIMUnsupportedCreate)
		r.Put("/Users/{id}", s.handleSCIMUnsupportedCreate)
		r.Delete("/Users/{id}", s.handleSCIMUnsupportedDelete)

		r.Patch("/Groups/{id}", s.handleSCIMPatchGroup)
		r.Post("/Groups", s.handleSCIMUnsupportedCreate)
		r.Delete("/Groups/{id}", s.handleSCIMUnsupportedDelete)
		// Everything else the invariant promises a 501 with a reason for.
		// Okta's group-push pre-check is `GET /Groups?filter=displayName eq
		// …`, and an unrouted path answered it with chi's text/plain 404.
		r.Get("/Groups", s.handleSCIMUnsupportedGroups)
		r.Get("/Groups/{id}", s.handleSCIMUnsupportedGroups)
		r.Put("/Groups/{id}", s.handleSCIMUnsupportedGroups)
	})

	// Auth routes (no auth required)
	r.Route("/api/v1/auth", func(r chi.Router) {
		// Sign-in provisions and links accounts, and those writes are audited
		// like any other authorization-relevant mutation — so the request
		// metadata has to be in context here too, not only on the API group.
		r.Use(requestMetaMiddleware)

		// What this install offers as a way in. Public: a login page must be
		// able to render before anyone is signed in.
		r.Get("/providers", s.handleLoginOptions)

		r.Delete("/session", s.handleLogout)
		// The one auth route that accepts an API key, so a script can verify
		// its token against something before assuming it works. Mounted with
		// With() rather than on the group: everywhere else here a presented
		// key is simply ignored and the caller is anonymous, which is the
		// right answer for a login form.
		r.With(s.apiKeyAuth).Get("/me", s.handleMe)

		// Local sign-in — the built-in administrator's password. Registered
		// unconditionally because whether it is offered is a database row an
		// admin controls, not a flag fixed at boot; both handlers answer 404
		// when it is switched off, so the surface looks absent rather than
		// merely unavailable.
		r.Post("/local/login", s.handlePasswordLogin)
		r.Post("/local/password", s.handlePasswordChange)

		// `toft login`. Three paths, and where they are mounted is half the
		// design: on this group a presented key is ignored and the caller is
		// anonymous, so **no credential can mint a credential** — a stolen toftu_
		// cannot start a flow and approve it. The start and the poll are
		// unauthenticated by necessity, since the terminal has nothing to
		// present yet; the lookup and the approval carry auth.Middleware
		// explicitly, exactly as /app-grant does, because those two are the
		// human's half and a browser session is the whole authorization for the
		// mint.
		// The short-lived key a trusted issuer's identity token buys (ADR
		// §157), and its holder's revocation. Here, where a presented key is
		// ignored, so no key can buy its own successor.
		r.Post("/token-exchange", s.handleTokenExchange)
		r.Post("/token-exchange/revoke", s.handleExchangedKeyRevoke)

		r.Post("/cli/device", s.handleCLIDeviceStart)
		r.Post("/cli/device/token", s.handleCLIDeviceToken)
		r.With(auth.Middleware(s.jwt, s.db, s.sessionOptions())).Get("/cli/device", s.handleCLIDeviceLookup)
		r.With(auth.Middleware(s.jwt, s.db, s.sessionOptions())).Post("/cli/device/approve", s.handleCLIDeviceApprove)

		// The two-host handshake's first hop, on the origin that holds the
		// viewer's session cookie. auth.Middleware explicitly, because this
		// group does not carry it: everywhere else here a caller is meant to be
		// anonymous, and this is the one route that needs to know who they are.
		// A nil principal is not an error — it means "sign in first", which the
		// handler turns into a login redirect that resumes the handshake.
		r.With(auth.Middleware(s.jwt, s.db, s.sessionOptions())).Get("/app-grant", s.handleAppGrant)

		// One pair of handlers for every provider. `github` is still a valid
		// provider id, so an install with GITHUB_CLIENT_ID set keeps the
		// callback URL it already registered at GitHub. Declared last so the
		// fixed paths above are never shadowed by a provider named after one.
		r.Get("/{provider}/login", s.handleProviderLogin)
		r.Get("/{provider}/callback", s.handleProviderCallback)
	})

	// Protected API routes — auth middleware populates Principal (nil = anonymous);
	// individual handlers decide via authorize().
	// Runner callbacks. Outside the authenticated group on purpose: the
	// session middleware has nothing to say about a workload, and routing
	// these through it would only create the temptation to accept a session
	// where a run-scoped token belongs. Each handler validates its own
	// audience, and the audiences are what keep app code from re-fetching
	// secrets.
	r.Route("/api/v1/runner", func(r chi.Router) {
		r.Post("/bootstrap", s.handleRunnerBootstrap)
		// Same credential as bootstrap — the one the cluster issued — and for
		// the same reason: it is the only one a workload holds that outlives
		// the token it is exchanging.
		r.Post("/token", s.handleRunnerToken)
		r.Post("/logs", s.handleRunnerLogs)
		r.Post("/consumption", s.handleRunnerConsumption)
		r.Post("/progress", s.handleRunnerProgress)
		// A run's output URL, minted at write time with the run token: the one
		// handed over at bootstrap may have expired by the end of a long run.
		r.Post("/output-url", s.handleRunnerOutputURL)
		r.Post("/result", s.handleRunnerResult)
	})

	// The app-facing surface. Outside the authenticated group for the same
	// reason /api/v1/runner is: the session middleware has nothing to say about
	// a workload, and routing these through it would only create the temptation
	// to accept a session where an app token belongs.
	//
	// Two rules hold here and nowhere else in this file. No route takes an app
	// id — the token *is* the app, which is why none of these handlers calls
	// auth.Authorize and why none of them can have an IDOR. And nothing but an
	// tofta_ token gets in: a cookie and an toftu_ key are both refused,
	// because this surface exists for an app to act as itself.
	r.Route("/api/v1/app", func(r chi.Router) {
		r.Use(s.appTokenAuth)
		r.Get("/self", s.handleAppSelf)
		r.Get("/url", s.handleAppURL)
		r.Get("/storage", s.handleAppSelfStorage)

		// The stores mapped to this app, reached over signed URLs: the only
		// route to one on a shared host, and beside the mount on a cluster.
		// `{slug}` is a resource, not an app id — the token is the app — and
		// the mapping, read per request, is the whole of the authority.
		r.Get("/store/{slug}/objects", s.handleAppListStoreObjects)
		r.Post("/store/{slug}/objects", s.handleAppStoreUpload)
		r.Get("/store/{slug}/objects/*", s.handleAppStoreDownload)
		r.Delete("/store/{slug}/objects/*", s.handleAppStoreDelete)
		r.Post("/store/{slug}/folders", s.handleAppStoreFolderCreate)
		// A storage link's address (store_link_handlers.go): a name at the
		// platform's address, opened at /files/ by store grants alone.
		r.Get("/store/{slug}/link/*", s.handleAppStoreLink)

		// Who is looking at this app. Takes no id — the act-for blob names the
		// viewer and the token names the app, so the pair is already scoped and
		// the IDOR rule has nothing to bite on. Off until an administrator
		// enables it for this app, which is why it can sit beside /self rather
		// than behind a second credential.
		r.Get("/viewer", s.handleAppViewer)

		// The app's one JSON document — settings and small state that outlive a
		// restart and a redeploy and belong to nobody's session. Needs no
		// attachment and no capability: it holds only what the app put there,
		// reaches nothing outside the install, and is the durable half of the
		// scratch directory every app already gets.
		r.Get("/data", s.handleAppDataGet)
		r.Put("/data", s.handleAppDataPut)
		r.Delete("/data", s.handleAppDataDelete)

		r.Get("/sends/{sendID}", s.handleAppSend)
		// The general form the catalog gives an app, and the three published
		// routes it does not replace. `{kind}` is not an app id, so the rule
		// that no route under /api/v1/app takes one is untouched.
		r.Post("/notify/{kind}", s.handleAppNotify)
		r.Post("/deliver/{kind}", s.handleAppDeliver)
		// The attached connection's own access token, for the provider's SDK —
		// the three file kinds only, and only while the connection allows it.
		r.Post("/tokens/{kind}", s.handleAppToken)
		// Published, called by both SDKs, and kept working permanently: an app
		// written against one release keeps running on the next. Same handler
		// bodies with the kind supplied rather than read from the URL.
		r.Post("/notify/slack", s.handleAppNotifySlack)
		r.Post("/notify/slack/file", s.handleAppNotifySlackFile)
		r.Post("/notify/email", s.handleAppNotifyEmail)

		// Background work: an app starting a different entrypoint of *itself*.
		// These take a run id rather than an app id, so the standing rule that
		// anything taking an ID from the URL scopes its query by it is back in
		// force — see app_job_handlers.go, where every one of them scopes by
		// the token's app inside the query.
		r.Post("/jobs/{name}/runs", s.handleAppStartJobRun)
		r.Get("/job-runs", s.handleAppListJobRuns)
		r.Get("/job-runs/{runID}", s.handleAppGetJobRun)
		r.Get("/job-runs/{runID}/logs", s.handleAppJobRunLogs)
		r.Get("/job-runs/{runID}/output", s.handleAppJobRunOutput)
		r.Post("/job-runs/{runID}/cancel", s.handleAppCancelJobRun)
	})

	// The git webhook, mounted **outside the /api/v1 subtree entirely** — above
	// apiKeyAuth, above the cookie middleware, above both route guards.
	//
	// It has no principal and needs none: the host presents an HMAC over the
	// body and nothing else, and there is no session, key or role for the guards
	// to reason about. Putting it inside would mean classifying a route where
	// the whole answer is "there is no credential of ours here", which is a
	// sentence that invites somebody to relax it later.
	r.Post("/api/v1/git/hooks/{connectionID}", s.handleGitHook)

	// The host data plane, outside /api/v1 for the webhook's reason: it reads
	// no cookie and no key, and the token in the path is its whole credential,
	// so there is nothing for the guards inside to reason about and nothing for
	// keyroutes.go to classify. On this router only — never AppsRouter — so a
	// token replayed at the apps listener finds no route.
	if s.hostStore != nil {
		r.Handle(hoststore.Route+"*", s.hostStore)
	}

	// Storage links (store_link_handlers.go), outside /api/v1 because a person
	// opens one in a browser from an email: the cookie session and nothing
	// else — no API key, which has /api/v1/store — and a missing session is a
	// sign-in redirect back here rather than a 401. Platform origin only.
	r.With(requestMetaMiddleware, auth.Middleware(s.jwt, s.db, s.sessionOptions())).
		Get(filesRoutePrefix+"{slug}/*", s.handleOpenStorageLink)

	r.Route("/api/v1", func(r chi.Router) {
		// Personal API keys, before the cookie middleware and never falling
		// back to it: an explicit credential beats an ambient one, and a
		// presented-but-unusable key must fail rather than quietly succeed as
		// whoever is signed in on that browser.
		r.Use(s.apiKeyAuth)
		// Deploy keys, beside personal keys and for the same reason. Order
		// between the two does not matter — the prefixes are disjoint, so at
		// most one of them ever finds a credential — but both must run before
		// the cookie middleware, or a pipeline whose token was revoked would
		// keep working from whatever session happened to be in the jar.
		r.Use(s.deployKeyAuth)
		r.Use(auth.Middleware(s.jwt, s.db, s.sessionOptions()))
		// Nothing under /apps answers a caller with no credential (BB-28,
		// BB-40): one guard for the subtree, so a route added later cannot
		// forget it the way /consumption did.
		r.Use(s.refuseAnonymousAppRoutes)
		r.Use(requestMetaMiddleware)
		// What a token may do, as opposed to what its owner may do. Runs after
		// authentication (it needs the principal) and before everything else
		// (a refused route should cost nothing further). Cookie sessions pass
		// straight through.
		r.Use(s.apiKeyRouteGuard)
		// The same idea for the third class, and for this one it is the entire
		// security boundary — a deploy key acts as owner on every app, so what
		// it may reach is decided here and nowhere else.
		r.Use(s.deployKeyRouteGuard)
		// Per-key traffic, counted around the handler so the response status
		// is known. In memory, like every other counter on a request path.
		r.Use(s.apiKeyUsage)
		// Presence is stamped for the platform API as well as the apps gateway:
		// someone reading the app list is online, and an operator asking "who is
		// here" would otherwise see only the people who happened to open an app.
		// Memory only — see internal/presence.
		r.Use(s.trackPresence)

		// The Posit Connect publishing surface, reached as /__api__ (see
		// connectAPIRewrite). Translations onto the app, upload and deployment
		// routes below, with their gates; see connect_handlers.go and ADR §109.
		r.Route("/connect", func(r chi.Router) {
			// Connect's error shape, not this API's envelope — see
			// error_envelope.go. Every helper these handlers share with the
			// rest of the API writes through writeJSON, so the opt-out is the
			// subtree's and not each call site's.
			r.Use(verbatimErrors)
			r.Get("/server_settings", s.handleConnectServerSettings)
			// rsconnect's token sign-in — RStudio's Add account dialog. Starts
			// an `toft login` approval; see connect_auth.go.
			r.Post("/tokens", s.handleConnectAddToken)
			// rsconnect's identity federation inside Posit Workbench: the
			// same exchange as /api/v1/auth/token-exchange (ADR §157). On
			// neither key allow-list, so a key cannot call it.
			r.Post("/v1/oauth/integrations/credentials", s.handleTokenExchange)
			r.Get("/users/current", s.handleConnectCurrentUser)
			r.Get("/v1/user", s.handleConnectCurrentUser)
			r.Get("/applications", s.handleConnectListApplications)
			r.Get("/applications/{appID}", s.handleConnectGetContent)
			r.Post("/v1/content", s.handleConnectCreateContent)
			r.Get("/v1/content/{appID}", s.handleConnectGetContent)
			r.Post("/v1/content/{appID}/bundles", s.handleConnectUploadBundle)
			r.Post("/v1/content/{appID}/deploy", s.handleConnectDeploy)
			r.Get("/v1/content/{appID}/environment", s.handleConnectGetEnvironment)
			r.Patch("/v1/content/{appID}/environment", s.handleConnectPatchEnvironment)
			r.Get("/v1/tasks/{taskID}", s.handleConnectGetTask)
		})

		// Slug availability, for the signed-in New App form. Inside the
		// authenticated group and refused anonymous: "taken" against "free"
		// told anyone which private apps exist (security review 2026-11-10,
		// B-3), undoing the gateway's same-404 rule.
		r.Get("/slugs/{slug}/available", s.handleCheckSlug)

		// One app by the name a person types. On this router rather than inside
		// the /apps subrouter below: that subrouter is a catch-all mount here,
		// and chi tries this router's param children (/apps/{appID}/sends,
		// /grants, …) before a catch-all, so inside it /apps/by-slug/sends ran
		// the sends list with appID "by-slug". A static child at the same node
		// beats the param one (security review 2026-11-14, RO-2).
		r.Get("/apps/by-slug/{slug}", s.handleGetAppBySlug)

		r.Route("/apps", func(r chi.Router) {
			r.Get("/", s.handleListApps)
			r.Post("/", s.handleCreateApp)
			r.Post("/upload", s.handleUploadApp)
			// `toft check` before `toft create`: the upload's decisions for an
			// app not yet made.
			r.Post("/check", s.handleCheckNewBundle)
			r.Route("/{appID}", func(r chi.Router) {
				r.Get("/", s.handleGetApp)
				r.Patch("/", s.handleUpdateApp)
				r.Delete("/", s.handleDeleteApp)
				// The environment's one write route: a merge, naming only what
				// changes (ADR §14).
				r.Patch("/env", s.handleUpdateAppEnv)

				r.Post("/deploy", s.handleDeploy)
				r.Delete("/garth-link", s.handleUnlinkGarth)
				r.Post("/upload", s.handleUploadVersion)
				// The upload's decisions without the upload: `toft check`.
				r.Post("/check", s.handleCheckBundle)
				r.Post("/deployments/{deployID}/rollback", s.handleRollback)
				r.Post("/restart", s.handleRestart)
				r.Post("/stop", s.handleStop)
				// A sleeping app, woken ahead of the visit an Open button is
				// about to make. The read rule, not ActionEditContent.
				r.Post("/wake", s.handleWake)
				// The platform lock. Its own pair of routes rather than a field
				// on PATCH, because everything else under this subtree
				// authorizes with ActionEditContent — which the owner has, and
				// a lock the owner can lift is not a lock. These two demand
				// system_admin inside the handler.
				r.Post("/lock", s.handleLockApp)
				r.Delete("/lock", s.handleUnlockApp)

				r.Get("/usage", s.handleAppUsage)
				// Named breakdown of the same traffic — manage_grants, not read.
				r.Get("/usage/viewers", s.handleAppUsageViewers)
				r.Get("/consumption", s.handleAppConsumption)

				// Background jobs. Definitions come from toft.toml; these
				// list them and pause a schedule. Runs are the history.
				r.Get("/jobs", s.handleListJobDefinitions)
				r.Patch("/jobs/{jobID}", s.handlePauseJob)
				r.Post("/jobs/{job}/runs", s.handleTriggerJobRun)

				r.Get("/job-runs", s.handleListJobRuns)
				r.Post("/job-runs", s.handleTriggerJobRun)
				r.Get("/job-runs/{runID}", s.handleGetJobRun)
				r.Post("/job-runs/{runID}/cancel", s.handleCancelJobRun)
				r.Post("/job-runs/{runID}/rerun", s.handleRerunJobRun)
				r.Get("/job-runs/{runID}/logs", s.handleJobRunLogs)
				r.Get("/job-runs/{runID}/output", s.handlePersonJobRunOutput)

				r.Put("/tags", s.handleSetAppTags)

				// What durable storage this app mounts. Read-only, and open to
				// anyone who can read the app: an owner needs to know their app
				// has /data/reports and what it may do there, and must not be
				// able to grant themselves another one.
				r.Get("/storage", s.handleAppStorage)
				// And the two that write it, for a caller who manages the
				// resource. Both halves are required — management on the
				// resource, ActionEditContent on the app — and both go through
				// AuthorizeAttach, which is the third of the three enforcement
				// points that may write storage_mappings.
				r.Put("/storage/{resourceID}", s.handleAttachAppStorage)
				r.Delete("/storage/{resourceID}", s.handleDetachAppStorage)

				// The app's own JSON document, from the platform side: whether
				// there is one, how big, when it changed — and a way to throw it
				// away. Never its contents; the app is the only reader and the
				// only writer. Reset is the escape hatch for an app that has
				// written state its own startup code cannot read, which a
				// redeploy by definition does not fix.
				r.Get("/data", s.handleAppDataMeta)
				r.Delete("/data", s.handleAppDataReset)

				// A document's re-render, which is a *deploy* and therefore
				// not on the Automation tab beside the jobs. Declared as
				// [render] in toft.toml: read, paused and given recipients
				// here, never written (ADR §144).
				r.Get("/render-schedule", s.handleGetRenderSchedule)
				r.Patch("/render-schedule", s.handlePatchRenderSchedule)
				r.Put("/render-schedule", s.handleRenderScheduleDeclared)
				r.Delete("/render-schedule", s.handleRenderScheduleDeclared)
				r.Put("/render-schedule/recipients", s.handlePutRenderScheduleRecipients)
				// The scheduled restart, `[restart]` in toft.toml: read and
				// paused here, never written.
				r.Get("/restart-schedule", s.handleGetRestartSchedule)
				r.Patch("/restart-schedule", s.handlePatchRestartSchedule)
				// What happened to each person on each run. The app's editors,
				// the same authorization the schedule itself has.
				r.Get("/render-schedule/deliveries", s.handleListReportDeliveries)

				// What has gone wrong with this app, and when. Gated like the
				// logs it excerpts, not like the app: viewers cannot read it.
				// Dependency policy (ADR §39). All three require can_edit,
				// enforced in the handlers: a viewer can do nothing with a
				// finding, and an exact package-and-version manifest of a
				// running internal tool is a target map.
				r.Get("/dependencies", s.handleAppDependencies)
				// One release's SBOM, CycloneDX, from the inventory the build
				// recorded; the same gate as the inventory itself.
				r.Get("/deployments/{deployID}/sbom", s.handleDeploymentSBOM)
				// Everything this app has been given, in one place. The same
				// gate as the dependency manifest beside it, for a stronger
				// version of the same reason: the assembly is worth more than
				// the sum of its parts, which is why a viewer keeps the
				// storage block on Settings and does not get this. ADR §92.
				r.Get("/reach", s.handleAppReach)
				// The app's audit file: reach's assembly plus its history, for
				// an editor or an administrator, each section gated as the
				// route it mirrors (app_audit_file_handlers.go).
				r.Get("/audit-file", s.handleAppAuditFile)
				r.Get("/audit-file/export", s.handleAppAuditFileExport)
				r.Post("/dependency-recheck", s.handleAppDependencyRecheck)
				// Granting a waiver is accepting risk on the organization's
				// behalf, so it is administrator-only and denied to keys.
				r.Post("/dependency-waivers", s.handleGrantDependencyWaiver)
				r.Delete("/dependency-waivers/{waiverID}", s.handleRevokeDependencyWaiver)

				r.Get("/events", s.handleListAppEvents)

				r.Get("/logs", s.handleLogs)
				r.Get("/logs/stream", s.handleLogStream)
				r.Get("/deployments", s.handleListDeployments)
				// Reclaiming storage: these spend disk and object-store
				// calls, not build capacity.
				r.Delete("/deployments", s.handleDeleteNonCurrentDeployments)
				r.Delete("/deployments/{deployID}", s.handleDeleteDeployment)
				r.Get("/deployments/{deployID}", s.handleGetDeployment)
				r.Get("/deployments/{deployID}/logs", s.handleDeploymentLogs)
				r.Get("/deployments/{deployID}/archive", s.handleDeploymentArchive)
				r.Get("/deployments/{deployID}/stream", s.handleDeployLogStream)
				// Where the deploy has got to, as opposed to what it printed.
				// Two streams because they are two different questions; see
				// deployment_progress_handlers.go.
				r.Get("/deployments/{deployID}/progress", s.handleDeployProgressStream)

				// The gallery attachment. Detaching is an ordinary edit of the
				// app; it takes no id but the app's own, which is the one in
				// this URL and is what every query below scopes by.
				r.Delete("/gallery", s.handleDetachGallery)
				// Taking the version the catalog now names: a deployment on
				// this same app, spending the same build capacity.
				r.Post("/gallery/update", s.handleGalleryUpdate)
			})
		})

		// The gallery: the catalog the Toft team publishes, and deploying
		// from it. Every route 404s while the mode is off — absent, not
		// disabled, like every other optional capability.
		//
		// Sync has its own once-a-minute bound, which is the service's and not
		// the router's.
		r.Route("/gallery", func(r chi.Router) {
			r.Get("/", s.handleListGallery)
			r.Post("/sync", s.handleGallerySync)
			r.Post("/{itemID}/deploy", s.handleGalleryDeploy)
		})

		// What this install tells an assessor. Admin-only, and every route
		// denied to every key class — see compliance_handlers.go for why this
		// is stricter than /admin/estate/reach.
		r.Route("/admin/compliance", func(r chi.Router) {
			r.Get("/", s.handleCompliance)
			r.Get("/controls", s.handleComplianceControls)
			r.Get("/report", s.handleComplianceReport)
			r.Get("/controls.pdf", s.handleComplianceControlsPDF)
			r.Get("/report/export", s.handleComplianceExport)
		})

		r.Get("/system/health", s.handleSystemHealth)
		// Ranked per-app consumption, scoped to what the caller may read.
		r.Get("/system/consumption", s.handleConsumptionSummary)
		// How many people opened an app in each window (system_admin only).
		r.Get("/system/active-users", s.handleActiveUsers)
		// Who is online right now, and every live workload — both system_admin
		// only, and both answers about this node rather than the install.
		r.Get("/system/presence", s.handlePresence)
		r.Get("/system/processes", s.handleProcesses)
		// The shared-mode agent's own figures, system_admin only; absent where
		// the shared runtime is not the agent.
		r.Get("/system/agent", s.handleSystemAgent)
		// The estate's four admin-wide reads: unfinished builds in the order
		// the worker will take them, every job run on the install, every app's
		// traffic, and every schedule. All system_admin only — each one
		// enumerates apps the caller may hold no grant on.
		r.Get("/system/queue", s.handleQueue)
		r.Get("/system/job-runs", s.handleSystemJobRuns)
		r.Get("/system/scheduled", s.handleScheduledOverview)
		r.Get("/system/scheduled/upcoming", s.handleUpcomingSchedules)
		r.Get("/system/usage", s.handleUsageOverview)
		r.Get("/system/audience", s.handleSystemAudience)
		// The other end of the same question: not who is busiest, but what
		// nobody has opened. Its own endpoint because it needs an *unwindowed*
		// last-visit, which /system/usage structurally cannot give — that one
		// caps MAX(day) at the window it charts, so every app quiet for longer
		// than the window reports the same empty answer.
		r.Get("/system/quiet-apps", s.handleQuietApps)
		// The install's self-diagnosis, computed per request and stored nowhere.
		r.Get("/system/checks", s.handleSystemChecks)
		// Emptying the shared build cache. A sibling of the report rather than
		// a check in it: the checks page is read-only diagnosis, so re-running
		// it can never reclaim anything, and this is the one destructive
		// maintenance verb the platform has. 202 and it starts; 409 if one is
		// already running, which is the only thing it refuses.
		r.Post("/system/build-cache/reclaim", s.handleReclaimBuildCache)
		// Where this install exports its telemetry, and whether that is
		// working. Admin-only and 404 to everyone else, like /system/settings.
		// A PUT applies immediately — the pipeline is rebuilt in place — which
		// is what makes this a settings page rather than a form that asks
		// somebody to restart the platform (ADR §31 D3).
		r.Get("/admin/telemetry-settings", s.handleGetTelemetrySettings)
		r.Put("/admin/telemetry-settings", s.handleUpdateTelemetrySettings)
		r.Post("/admin/telemetry-settings/test", s.handleTestTelemetry)
		// Features: what this build offers, what the install is licensed for,
		// and what an administrator has switched on. Admin-only and 404 to
		// everyone else, like /system/settings — which features an install has
		// bought is not an app owner's business.
		r.Get("/admin/features", s.handleListFeatures)
		r.Put("/admin/features/{name}", s.handleUpdateFeature)

		// Platform settings: what an administrator may turn on, as opposed to
		// what an operator's configuration makes possible.
		r.Get("/system/settings", s.handleGetSettings)
		r.Put("/system/settings", s.handleUpdateSettings)

		// Where this install will deploy from at all. Admin-only and 404 to
		// everyone else, like /system/settings — except /effective, which every
		// signed-in user may read, because the New App form has to be able to
		// say what *would* work rather than refusing blindly.
		// Admin → Dependencies (ADR §39). The estate view, the "which of our
		// apps has this" search, the warn → block preview, and the two
		// refresh buttons.
		r.Get("/admin/dependencies", s.handleAdminDependencies)
		r.Get("/admin/dependencies/search", s.handleDependencySearch)
		// The same inventory read for governance rather than exposure: with a
		// `q` the apps carrying a package whether or not anything is filed
		// against it, without one the estate's package list. A second route
		// rather than a flag on the search above, because a row here is an
		// (app, package, version) and there a row is an app with findings.
		r.Get("/admin/packages", s.handlePackageSearch)
		// One app's scan history — the governance read, and the second half of
		// the two-reader split the owner's tab is the first half of. Under
		// /admin because the question it answers is longitudinal and therefore
		// about response, not about what to fix.
		r.Get("/admin/apps/{appID}/dependency-scans", s.handleAppDependencyScans)
		// Admin → Estate. The estate by what it is *written in*, which is a
		// different question from the framework count on /tags/framework and
		// has a different reader: that page answers "show me the Flask apps I
		// can see" and stays viewer-scoped, this one answers "what breaks when
		// a series goes end-of-life" and is administrator-only.
		r.Get("/admin/estate/languages", s.handleEstateLanguages)
		// The same estate as an address list, for the team that has to point a
		// scanner at it. A third route rather than a flag on the census: a row
		// there is a language and a count, a row here is an app to visit.
		//
		// The one read on this subtree a deploy key may make — see
		// canReadEstateTargets for why that widening buys a column and no more.
		r.Get("/admin/estate/targets", s.handleEstateTargets)
		// The same estate as *what each app holds*, and the transpose of that:
		// which apps hold each credential, and each store. A third route
		// rather than a column on either of the two above — a row there is a
		// language and a count, or an app to visit; a row here is an app and
		// what it was given.
		//
		// Administrator only, and unlike its neighbour **not** a deploy key: a
		// pipeline credential has no business enumerating what every app on
		// the install holds. ADR §92 D12.
		r.Get("/admin/estate/reach", s.handleEstateReach)
		// The router's own account of itself, for a scanner that has no
		// checkout of this repository to walk. Admin-only for the reason
		// handleOpenAPI states: it is the whole surface in one response.
		r.Get("/admin/openapi.json", s.handleOpenAPI)
		r.Post("/admin/dependencies/preview", s.handleDependencyPreview)
		r.Post("/admin/dependency-recheck", s.handleDependencyRecheckAll)
		r.Post("/admin/dependency-feed/sync", s.handleDependencyFeedSync)
		// Write-only, like every credential on this platform: there is no read
		// route, and the estate response carries a boolean.
		r.Put("/admin/dependency-feed/credential", s.handleSetDependencyFeedCredential)

		r.Get("/system/source-policy", s.handleGetSourcePolicy)
		r.Put("/system/source-policy", s.handleUpdateSourcePolicy)
		// What a proposed policy would do. Mutates nothing, and nothing is
		// saved until an administrator has seen it.
		r.Post("/system/source-policy/preview", s.handlePreviewSourcePolicy)
		// What the install already deploys from, for seeding the list.
		r.Get("/system/source-policy/observed", s.handleObservedSources)
		r.Get("/system/source-policy/effective", s.handleEffectiveSourcePolicy)

		// The connections a developer may point an app at. Beside the effective
		// source policy above and readable by every signed-in user for the same
		// reason: a New App form that refuses without naming what would work
		// sends every developer to the same Slack channel.
		//
		// **It never carries the auth method.** Whether the organization's
		// connection is backed by a GitHub App or by a pasted token is an
		// administrator's fact about a credential; `picker` is the only part of
		// that difference a developer sees.
		r.Get("/git/connections", s.handleListMyGitConnections)

		// The picker. Its state cookie lives at /api/v1/git, against the
		// sign-in flow's /api/v1/auth — the browser is what keeps a
		// repository-scoped callback out of the sign-in handler, which is a
		// stronger separation than two handlers that agree not to share.
		r.Get("/git/connections/{connectionID}/authorize", s.handleGitAuthorize)
		r.Get("/git/connections/{connectionID}/callback", s.handleGitCallback)
		r.Get("/git/connections/{connectionID}/repos", s.handleListConnectionRepos)

		// Directory search for pickers — authenticated users only, minimal DTO
		r.Get("/directory/users", s.handleDirectoryUsers)
		r.Get("/directory/groups", s.handleDirectoryGroups)

		// User administration
		r.Get("/users", s.handleListUsers)
		r.Get("/users/{id}", s.handleGetUser)
		// Only for an account that owns nothing — see handleDeleteUser.
		r.Delete("/users/{id}", s.handleDeleteUser)
		r.Patch("/users/{id}/role", s.handleUpdateUserRole)
		r.Post("/users/{id}/lock", s.handleLockUser)
		r.Post("/users/{id}/unlock", s.handleUnlockUser)
		// The scanner mark: believed for X-Toft-Scan, admitted to the estate
		// read, never a grant — see handleSetScanner.
		r.Post("/users/{id}/scanner", s.handleSetScanner(true))
		r.Delete("/users/{id}/scanner", s.handleSetScanner(false))
		// Letting in an account a provider created but did not finish.
		r.Post("/users/{id}/activate", s.handleActivateUser)
		r.Get("/users/{id}/access", s.handleUserAccess)
		// What they have been opening — the transpose of each app's viewers.
		r.Get("/users/{id}/app-usage", s.handleUserAppUsage)
		// Offboarding in one action, and the migration path for an install
		// adopting group ownership — see handleTransferUserApps.
		r.Post("/users/{id}/transfer-apps", s.handleTransferUserApps)

		// Deploy keys — the credential that belongs to nobody. Admin-only, and
		// denied to both other credential classes: no key mints a key.
		r.Get("/deploy-keys", s.handleListDeployKeys)
		r.Post("/deploy-keys", s.handleCreateDeployKey)
		r.Patch("/deploy-keys/{keyID}", s.handleUpdateDeployKey)
		r.Delete("/deploy-keys/{keyID}", s.handleRevokeDeployKey)

		// Grants
		r.Get("/apps/{appID}/grants", s.handleListGrants)
		r.Put("/apps/{appID}/grants/{principalType}/{principalID}", s.handlePutGrant)
		r.Delete("/apps/{appID}/grants/{principalType}/{principalID}", s.handleDeleteGrant)

		// Visibility change is routed via PATCH /apps/{appID} (handleUpdateApp).

		// Groups
		r.Get("/groups", s.handleListGroups)
		r.Post("/groups", s.handleCreateGroup)
		r.Get("/groups/{id}", s.handleGetGroup)
		r.Patch("/groups/{id}", s.handleUpdateGroup)
		r.Delete("/groups/{id}", s.handleDeleteGroup)
		r.Put("/groups/{id}/members/{userId}", s.handleAddGroupMember)
		r.Delete("/groups/{id}/members/{userId}", s.handleRemoveGroupMember)
		// Where a group's membership may come from a directory (ADR §51).
		// Admin-only, unlike the rest of this block — see
		// requireGroupMappingAdmin.
		r.Get("/groups/{id}/claim-mappings", s.handleListGroupClaimMappings)
		r.Put("/groups/{id}/claim-mappings", s.handleAddGroupClaimMapping)
		r.Delete("/groups/{id}/claim-mappings", s.handleRemoveGroupClaimMapping)

		// Introspection
		r.Get("/apps/{appID}/permissions", s.handleAppPermissions)
		r.Get("/me/capabilities", s.handleMyCapabilities)

		// The account's view of itself. The self-scoped halves of
		// GET /users/{id} and GET /users/{id}/access — no id in either path,
		// which is what keeps the admin routes' gate unconditional. See the
		// header of me_handlers.go.
		r.Get("/me/account", s.handleMyAccount)
		r.Get("/me/access", s.handleMyAccess)
		// Which storage resources this caller may attach to an app, and at what
		// ceiling. `/me/*` rather than `/storage/*` because there is no
		// `/storage` prefix on this API and this is the caller's own answer, as
		// `/me/capabilities` and `/me/access` are. It carries no bucket and no
		// prefix: those are the containment boundary and an administrator's.
		r.Get("/me/storage", s.handleMyAttachableStorage)
		// The second read path audit_log has ever had, and a keyhole rather
		// than a door: the caller's own rows, the session.* family, a fixed
		// window and a fixed cap. See handleMySignins.
		r.Get("/me/sign-ins", s.handleMySignins)
		// Ending every session you have, everywhere. The mechanism is the same
		// token_version bump a demotion or a lock performs.
		r.Post("/me/sessions/revoke", s.handleRevokeMySessions)
		// The zone this person reads times in (ADR §154). GET /auth/me carries
		// the value back, beside the install's default.
		r.Put("/me/time-zone", s.handleSetMyTimeZone)

		// Access requests (object-level)
		r.Post("/apps/{appID}/access-requests", s.handleCreateAccessRequest)
		r.Get("/apps/{appID}/access-requests", s.handleListAccessRequests)
		r.Post("/apps/{appID}/access-requests/{reqID}/approve", s.handleApproveAccessRequest)
		r.Post("/apps/{appID}/access-requests/{reqID}/deny", s.handleDenyAccessRequest)
		r.Delete("/apps/{appID}/access-requests/{reqID}", s.handleWithdrawAccessRequest)

		// Ownership transfer
		r.Post("/apps/{appID}/transfer", s.handleTransferOwnership)

		// Tags — a tag is a key and an app holds one value for it. Admins
		// define the keys; users type the values. The Framework tag is
		// system-owned: its values come from apps.framework and cannot be set.
		r.Get("/tags", s.handleListTags)
		r.Get("/tags/{tagSlug}/values", s.handleListTagValues)
		// Sign-in providers. Admin-only, and the reason is the whole feature:
		// whoever can add a provider can add a door they control.
		// The merged governance-and-system feed. The first read path audit_log
		// has ever had; system_admin only, and 404 to everyone else.
		r.Get("/admin/audit", s.handleListActivity)

		r.Get("/admin/trusted-issuers", s.handleListTrustedIssuers)
		r.Post("/admin/trusted-issuers", s.handleCreateTrustedIssuer)
		r.Patch("/admin/trusted-issuers/{issuerID}", s.handleUpdateTrustedIssuer)
		r.Delete("/admin/trusted-issuers/{issuerID}", s.handleDeleteTrustedIssuer)
		r.Get("/admin/exchanged-keys", s.handleListExchangedKeys)
		r.Delete("/admin/exchanged-keys/{keyID}", s.handleRevokeExchangedKey)
		r.Get("/admin/auth-providers", s.handleListAuthProviders)
		r.Post("/admin/auth-providers", s.handleCreateAuthProvider)
		r.Patch("/admin/auth-providers/{providerID}", s.handleUpdateAuthProvider)
		r.Delete("/admin/auth-providers/{providerID}", s.handleDeleteAuthProvider)
		r.Post("/admin/auth-providers/discover", s.handleDiscoverIssuer)

		// Credentials an administrator holds and attaches to apps. Under
		// /admin including the two that name an app: attaching is not something
		// an app's owner may do, and a route living beside the ones they can
		// reach is a route somebody eventually opens up.
		r.Get("/admin/credentials", s.handleListCredentials)
		r.Post("/admin/credentials", s.handleCreateCredential)
		r.Patch("/admin/credentials/{id}", s.handleUpdateCredential)
		r.Get("/admin/credentials/{id}/apps", s.handleListCredentialApps)
		r.Delete("/admin/credentials/{id}", s.handleDeleteCredential)
		r.Put("/admin/apps/{appID}/credentials/{id}", s.handleAttachCredential)
		r.Delete("/admin/apps/{appID}/credentials/{id}", s.handleDetachCredential)
		// The SCIM feed's credential. Minted here and presented at /scim/v2,
		// which is the only place it is any use.
		r.Post("/admin/auth-providers/{providerID}/scim-token", s.handleMintSCIMToken)
		r.Delete("/admin/auth-providers/{providerID}/scim-token", s.handleRevokeSCIMToken)

		// Connected git hosts. Beside /admin/auth-providers because that is
		// this codebase's convention for an admin-managed, secret-holding
		// connection row — and they share the Sources page with the source
		// policy rather than a prefix, because between them they answer one
		// question: where may code come from, and how do we reach it.
		//
		// Secrets are write-only in the DTO. /apps is what the page reads to
		// show an administrator what a removal would refuse on, *before* they
		// try it.
		r.Get("/admin/git-connections", s.handleListGitConnections)
		r.Post("/admin/git-connections", s.handleCreateGitConnection)
		r.Patch("/admin/git-connections/{connectionID}", s.handleUpdateGitConnection)
		r.Delete("/admin/git-connections/{connectionID}", s.handleDeleteGitConnection)
		r.Get("/admin/git-connections/{connectionID}/apps", s.handleListGitConnectionApps)
		r.Post("/admin/git-connections/{connectionID}/test", s.handleTestGitConnection)
		r.Post("/admin/git-connections/{connectionID}/webhook-secret", s.handleRotateGitConnectionWebhookSecret)
		r.Delete("/admin/git-connections/{connectionID}/webhook-secret", s.handleDisableGitConnectionWebhook)

		r.Post("/admin/tags", s.handleCreateTag)
		r.Patch("/admin/tags/{tagID}", s.handleUpdateTag)
		r.Delete("/admin/tags/{tagID}", s.handleDeleteTag)

		// Durable app storage. Admin-only, mirroring /admin/tags: an
		// administrator names the durable locations and decides which apps
		// reach them. There is no self-service provisioning — a volume is a
		// real cost against a filesystem with a hard access-point quota.
		// Every route 404s when the install offers no storage at all.
		r.Get("/admin/storage", s.handleListStorageResources)
		r.Post("/admin/storage", s.handleCreateStorageResource)
		r.Get("/admin/storage/defaults", s.handleListStorageDefaults)
		r.Put("/admin/storage/defaults", s.handlePutStorageDefault)
		r.Delete("/admin/storage/defaults/{subject}", s.handleDeleteStorageDefault)
		// What it is *called*, and never where it lives — a slug in the body is
		// refused by name, because every mounted binding is derived from it.
		r.Patch("/admin/storage/{id}", s.handleRenameStorageResource)
		r.Patch("/admin/storage/drives/{key}", s.handlePutStorageDriveLabel)
		r.Post("/admin/storage/drives/{key}/repoint", s.handleRepointStorageDrive)
		r.Delete("/admin/storage/{id}", s.handleDeleteStorageResource)
		// A name a finished delete holds because its volume's files were kept.
		r.Delete("/admin/storage/deleting/{slug}", s.handleReleaseKeptStorageName)
		r.Get("/admin/storage/{id}/mappings", s.handleListStorageMappings)
		r.Put("/admin/storage/{id}/mappings/{appID}", s.handlePutStorageMapping)
		r.Delete("/admin/storage/{id}/mappings/{appID}", s.handleDeleteStorageMapping)

		// Who may attach this resource without being an administrator, and at
		// what ceiling. The delete is the only route in the product that
		// restarts somebody else's app as a consequence of a permission
		// change: revoking takes the mappings the delegation wrote, and the
		// apps carrying them come back onto their current release without the
		// mount. See internal/api/storage_manager_handlers.go.
		r.Get("/admin/storage/{id}/managers", s.handleListStorageManagers)
		r.Put("/admin/storage/{id}/managers", s.handlePutStorageManager)
		r.Delete("/admin/storage/{id}/managers/{subject}", s.handleDeleteStorageManager)

		// Who may reach a store from *outside*. A mapping is an app's and a
		// grant is a person's; neither implies the other, which is why these
		// are three routes beside the three above rather than a field on them.
		r.Get("/admin/storage/{id}/grants", s.handleListStoreGrants)
		r.Put("/admin/storage/{id}/grants", s.handlePutStoreGrant)
		r.Delete("/admin/storage/{id}/grants/{subject}", s.handleDeleteStoreGrant)

		// The store's own surface: what this caller can reach, and the signed
		// URLs that let them use it. Not under /admin — a grant holder is not
		// an administrator, and this is the one storage surface that is not.
		// Every route 404s unless the install has both a storage backend and a
		// signer.
		r.Get("/store", s.handleListStores)
		r.Get("/store/{slug}/objects", s.handleListStoreObjects)
		r.Post("/store/{slug}/objects", s.handleStoreUpload)
		r.Get("/store/{slug}/objects/*", s.handleStoreDownload)
		r.Delete("/store/{slug}/objects/*", s.handleStoreDelete)
		r.Post("/store/{slug}/folders", s.handleStoreFolderCreate)
		r.Delete("/store/{slug}/folders/*", s.handleStoreFolderDelete)

		// The app SDK this install carries, for `toft sdk vendor`. Not a
		// registry and not anonymous: see internal/api/sdk_handlers.go.
		r.Get("/sdk/{lang}", s.handleGetSDK)

		// Announcement banner
		r.Get("/announcement", s.handleGetAnnouncement)
		r.Put("/admin/announcement", s.handlePutAnnouncement)
		r.Delete("/admin/announcement", s.handleDeleteAnnouncement)

		// The announcement an administrator *sends* — the banner's neighbour
		// and nothing like it. The banner is one idempotent string that DELETE
		// takes back; this reaches every person on the install, cannot be taken
		// back, and leaves a permanent row. Admin-only, 404 to everybody else,
		// and refused to every API key by name in keyroutes.go.
		//
		// `estimate` is mounted before `{id}` reads it, and chi prefers the
		// static segment; it is a live count the composer asks for again every
		// time the audience switch moves.
		r.Get("/admin/announcement-emails", s.handleListAnnouncementEmails)
		r.Post("/admin/announcement-emails", s.handleCreateAnnouncementEmail)
		r.Get("/admin/announcement-emails/estimate", s.handleEstimateAnnouncementAudience)
		r.Get("/admin/announcement-emails/{id}", s.handleGetAnnouncementEmail)
		r.Patch("/admin/announcement-emails/{id}", s.handleUpdateAnnouncementEmail)
		r.Delete("/admin/announcement-emails/{id}", s.handleDeleteAnnouncementEmail)
		r.Post("/admin/announcement-emails/{id}/clone", s.handleCloneAnnouncementEmail)
		r.Post("/admin/announcement-emails/{id}/test", s.handleTestAnnouncementEmail)
		r.Post("/admin/announcement-emails/{id}/send", s.handleSendAnnouncementEmail)
		r.Post("/admin/announcement-emails/{id}/resend", s.handleResendAnnouncementEmail)

		// The install's own knowledge base — one app an administrator points
		// everybody at. The resolving route is signed-in only and answers `{}`
		// to anyone the picked app is not readable by; the administrator's own
		// read is separate because it must be able to say "this names an app
		// that no longer exists". See knowledge_base_handlers.go.
		r.Get("/knowledge-base", s.handleGetKnowledgeBase)
		r.Get("/admin/knowledge-base", s.handleGetAdminKnowledgeBase)
		r.Put("/admin/knowledge-base", s.handlePutKnowledgeBase)
		r.Delete("/admin/knowledge-base", s.handleDeleteKnowledgeBase)

		// Toft's own manuals — the apps on the reserved `_` namespace,
		// which the app list drops and this route gathers. No admin variant
		// and no stored value: it is the complement of `GET /apps`, read
		// through the same `listAppsFor`. See documentation_handlers.go.
		r.Get("/documentation", s.handleListDocumentation)

		// Email. Admin-only and 404 to everyone else, like /system/settings:
		// which relay this install sends through is infrastructure detail.
		r.Get("/admin/email-settings", s.handleGetEmailSettings)
		r.Put("/admin/email-settings", s.handleUpdateEmailSettings)
		r.Post("/admin/email-settings/test", s.handleTestEmail)

		// The integration catalog. Admin-only and 404 to everyone else for the
		// same reason email is: which workspace, which site and which relay
		// this install reaches is infrastructure detail.
		//
		// Every /admin/integrations route is on the *denied* list in
		// keyroutes.go. A connection is a credential, and the standing rule is
		// that a key never manages one — eight kinds does not soften it.
		r.Get("/admin/integrations/connections", s.handleListConnections)
		r.Post("/admin/integrations/connections", s.handleCreateConnection)
		r.Patch("/admin/integrations/connections/{connID}", s.handleUpdateConnection)
		r.Delete("/admin/integrations/connections/{connID}", s.handleDeleteConnection)
		r.Post("/admin/integrations/connections/{connID}/test", s.handleTestConnection)
		r.Get("/admin/integrations/connections/{connID}/destinations", s.handleConnectionDestinations)
		// What this connection has left in somebody else's system. The only
		// version of "clean this up" that is safe: the platform will not issue
		// bulk deletes inside an organization's document store on the strength
		// of rows it wrote itself.
		r.Get("/admin/integrations/connections/{connID}/placements", s.handleConnectionPlacements)
		// The Teams bot's pasted teams and chats, and the app package an
		// administrator uploads in the Teams admin center.
		r.Get("/admin/integrations/connections/{connID}/places", s.handleListPlaces)
		r.Post("/admin/integrations/connections/{connID}/places", s.handleAddPlace)
		r.Delete("/admin/integrations/connections/{connID}/places/{placeID}", s.handleDeletePlace)
		r.Get("/admin/integrations/connections/{connID}/teams-app", s.handleTeamsAppPackage)

		// How long the ledger is kept.
		r.Get("/admin/integration-settings", s.handleGetIntegrationSettings)
		r.Put("/admin/integration-settings", s.handleUpdateIntegrationSettings)
		// The send ledger. Kept working while every connection is switched off,
		// for the reason /admin/api-keys is: that is exactly when a leaked one
		// has to be found.
		r.Get("/admin/integration-sends", s.handleAdminIntegrationSends)

		// App integrations. Reading and the app-team-owned fields are
		// ActionEditContent; attaching and detaching are admin-only and checked
		// inside the handler — attaching email lets an app mail any address on
		// the internet carrying this install's sending identity, which is not an
		// app owner's grant to make.
		r.Get("/apps/{appID}/integrations", s.handleGetAppIntegrations)
		r.Get("/apps/{appID}/sends", s.handleListAppSends)
		r.Post("/apps/{appID}/integrations/test", s.handleTestAppIntegration)
		r.Put("/apps/{appID}/integrations/{kind}", s.handleAttachAppIntegration)
		r.Delete("/apps/{appID}/integrations/{kind}", s.handleDetachAppIntegration)
		// The app's own half: where inside the administrator's scope its
		// messages go. Authorized against the app, not the install — anyone who
		// may change the app's settings may choose, once an administrator has
		// attached it.
		r.Put("/apps/{appID}/integrations/{kind}/destination", s.handleSetAppIntegrationDestination)
		r.Get("/apps/{appID}/integrations/{kind}/destinations", s.handleAppIntegrationDestinations)

		// Personal API keys — a credential for automation outside Toft.
		// Every route here is on the denied list: a token must not be able to
		// mint or revoke tokens, its own included.
		r.Get("/me/api-keys", s.handleListMyAPIKeys)
		r.Post("/me/api-keys", s.handleCreateMyAPIKey)
		r.Patch("/me/api-keys/{keyID}", s.handleUpdateMyAPIKey)
		r.Delete("/me/api-keys/{keyID}", s.handleDeleteMyAPIKey)
		r.Get("/me/api-keys/{keyID}/usage", s.handleMyAPIKeyUsage)

		// The install-wide inventory. An administrator who learns a token has
		// leaked needs to find and kill it from a screen, not from psql.
		r.Get("/admin/api-keys", s.handleListAllAPIKeys)
		r.Patch("/admin/api-keys/{keyID}", s.handleAdminUpdateAPIKey)
		r.Delete("/admin/api-keys/{keyID}", s.handleAdminDeleteAPIKey)

		// The same credentials narrowed to one account, for its record. They
		// live here rather than beside the other /users routes because what
		// governs them is the credential rules — admin-gated, denied to keys,
		// alive while the capability is off — not the account rules.
		r.Get("/users/{id}/api-keys", s.handleListUserAPIKeys)
		r.Get("/users/{id}/api-keys/{keyID}/usage", s.handleUserAPIKeyUsage)

		// System role promotion workflow
		r.Get("/me/access-requests", s.handleListMyAccessRequests)
		r.Post("/me/promotion-requests", s.handleCreatePromotionRequest)
		r.Get("/me/promotion-requests", s.handleListMyPromotionRequests)
		r.Delete("/me/promotion-requests/{reqID}", s.handleWithdrawMyPromotionRequest)
		r.Get("/admin/promotion-requests", s.handleListPromotionRequests)
		r.Post("/admin/promotion-requests/{reqID}/approve", s.handleApprovePromotionRequest)
		r.Post("/admin/promotion-requests/{reqID}/deny", s.handleDenyPromotionRequest)
	})

	if s.cfg.SeparateAppsListener() {
		// Deployed apps live on their own origin. Anything arriving here for a
		// real app slug is an old link — send it across; everything else is an
		// SPA route such as /apps/{id}.
		r.Route("/apps/{slug}", func(r chi.Router) {
			r.HandleFunc("/*", s.redirectToAppsGateway)
			r.HandleFunc("/", s.redirectToAppsGateway)
		})
	} else {
		// Single-origin fallback: serve apps inline. This gives up the
		// cross-origin isolation the split provides — including this: a slug
		// that resolves to nothing falls through to the SPA here, because on
		// this origin the SPA is what `/apps/...` otherwise means.
		r.Route("/apps/{slug}", func(r chi.Router) {
			r.Use(s.appProxyGuard(spaHandler.ServeHTTP))
			r.HandleFunc("/*", s.handleAppProxy)
			r.HandleFunc("/", s.handleAppProxy)
		})
	}

	// Serve the SPA for all other routes — except under /api/, where a client
	// asked a program a question and an HTML page is not an answer to it.
	r.NotFound(s.notFound(spaHandler.ServeHTTP))
	// And a method the route does not take, in the same envelope: chi's
	// default 405 is an empty body, so `code` was absent from the one refusal
	// a client gets for a wrong verb (CLI.28 — never absent).
	r.MethodNotAllowed(s.methodNotAllowed)

	// Kept for apiKeyRouteGuard's pattern lookup. Assigned last, when every
	// route is registered — Match against a half-built router would resolve
	// nothing and refuse everything.
	s.mux = r
	s.muxRoutes = servedRoutes(r)

	return r
}

// servedRoutes is every method+pattern pair the router has a handler for, in
// the spelling routeMatch produces.
func servedRoutes(mux *chi.Mux) map[string]bool {
	out := map[string]bool{}
	_ = chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		out[method+" "+normalizeRoutePattern(route)] = true
		return nil
	})
	return out
}

// AppsRouter is the handler for the apps gateway listener. It serves deployed
// apps and nothing else — no SPA, no platform API — so that code running on
// this origin has no same-origin path to Toft itself.
func (s *Server) AppsRouter() http.Handler {
	r := chi.NewRouter()

	// The gateway logs too, and that was a deliberate reversal — ADR §46. The
	// data-path invariant forbids a DB *write* on this path and this is a log
	// line, but §45 had also said "who reached which app" would never be a
	// per-request record, and this makes it one. The volume and the retention
	// are the operator's to carry; the entry says so.
	r.Use(accessLog(telemetry.SurfaceApp))
	r.Use(middleware.Recoverer)
	if len(s.cfg.TrustedProxyCIDRs) > 0 {
		r.Use(realIPMiddleware(s.cfg.TrustedProxyCIDRs))
	}
	// The gateway's own surface. Separate series from the platform's, because
	// "is the platform healthy" and "is somebody's Flask app healthy" have
	// different answers and different owners — and on a single-origin install
	// (ADR §15), where this router is not mounted at all, the surface attribute
	// is what still keeps them apart.
	r.Use(telemetry.Middleware(telemetry.SurfaceApp, s.inboundTraceTrusted))
	// HSTS on everything this host serves, the gateway's own cards and every
	// refusal included, so ahead of the origin check and the reserved paths.
	// After realIP, whose verdict the scheme test reads.
	r.Use(s.hsts(s.cfg.AppsBaseURL))

	if s.cfg.DevPassword != "" {
		r.Use(s.devBasicAuth)
	}
	// Before the apps origin check, which is the shared origin's and not an
	// app host's: a match is served here, under its own check, and never
	// reaches the routes below.
	r.Use(s.appHostDispatch)
	r.Use(s.appsOriginCheck)

	// The reserved path, declared before app routing so nothing an app serves
	// can shadow it. This is the only thing on the apps origin that is not an
	// app, and it exists because a cookie can only be set by the origin it
	// belongs to (internal/api/app_handshake.go).
	r.Get(appHandshakePath, s.handleAppSessionRedeem)

	// The second reserved path, declared here for the same reason and serving
	// the same file as the platform origin's. A researcher who found something
	// in a deployed app is looking at *this* origin and has no reason to know
	// there is another one.
	r.Get(securityTxtPath, s.handleSecurityTxt)

	r.Route("/apps/{slug}", func(r chi.Router) {
		// A slug that resolves to nothing is 404 on this origin, never the
		// SPA — the same answer the NotFound below gives every other unknown
		// path here, and for the reason in the comment on gatewayUnresolved.
		r.Use(s.appProxyGuard(s.gatewayUnresolved))
		r.HandleFunc("/*", s.handleAppProxy)
		r.HandleFunc("/", s.handleAppProxy)
	})

	r.NotFound(s.gatewayUnresolved)

	return r
}

// redirectToAppsGateway forwards a legacy platform-origin app link to the apps
// listener, preserving path and query.
func (s *Server) redirectToAppsGateway(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	app, _ := s.db.GetAppBySlug(slug)
	if app == nil {
		// Not a deployed app — an SPA route such as /apps/{id}.
		s.spaHandler.ServeHTTP(w, r)
		return
	}

	// An app whose release lives at the root of its own hostname is sent
	// straight there, rather than through the gateway's own 308.
	if builder.ServingBasePath(app) == "" && app.Hostname != "" {
		http.Redirect(w, r, s.appHostURL(app, strings.TrimPrefix(r.URL.EscapedPath(), builder.BasePath(slug)), r.URL.RawQuery),
			http.StatusPermanentRedirect)
		return
	}

	// The path as sent: a decoded one turns a%3Fb into a query (HD-9).
	target := s.cfg.AppsBaseURL + r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusTemporaryRedirect)
}

const devAuthCookie = auth.CookiePrefix + "dev_auth"
const devAuthMaxAge = 30 * 24 * 60 * 60 // 30 days

// devBasicAuth gates all routes behind HTTP Basic Auth with a long-lived cookie.
// On first visit the browser shows its native login dialog (user: admin).
// Once authenticated, a 30-day cookie is set so the browser stays logged in
// across sessions and GitHub OAuth redirects work without re-prompting.
func (s *Server) devBasicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check for existing valid cookie first
		if c, err := r.Cookie(devAuthCookie); err == nil {
			if s.validDevToken(c.Value, time.Now()) {
				next.ServeHTTP(w, r)
				return
			}
		}

		// Fall back to Basic Auth prompt
		user, pass, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(user), []byte("admin")) != 1 ||
			subtle.ConstantTimeCompare([]byte(pass), []byte(s.cfg.DevPassword)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="Toft"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Credentials valid — set long-lived cookie
		now := time.Now()
		http.SetCookie(w, &http.Cookie{
			Name:     devAuthCookie,
			Value:    s.devToken(now.Add(devAuthMaxAge * time.Second)),
			Path:     "/",
			MaxAge:   devAuthMaxAge,
			Expires:  now.Add(devAuthMaxAge * time.Second),
			Secure:   s.secureCookies(),
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
		next.ServeHTTP(w, r)
	})
}

// devToken is the gate cookie's value: an expiry and an HMAC-SHA256 of it keyed
// by the dev password. Changing DEV_PASSWORD invalidates all existing cookies,
// and the expiry is signed so a captured cookie dies with its thirty days — a
// fixed message made one value that opened the gate for as long as the
// password lived, in every browser alike.
func (s *Server) devToken(exp time.Time) string {
	e := strconv.FormatInt(exp.Unix(), 10)
	return e + "." + s.devTokenMAC(e)
}

func (s *Server) devTokenMAC(exp string) string {
	mac := hmac.New(sha256.New, []byte(s.cfg.DevPassword))
	mac.Write([]byte("toft-dev-auth|" + exp))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) validDevToken(token string, now time.Time) bool {
	e, sig, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(e, 10, 64)
	if err != nil || now.Unix() >= exp {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(sig), []byte(s.devTokenMAC(e))) == 1
}

// notFound answers an unmatched path: JSON under /api/, and the SPA everywhere
// else, which is what makes a deep link into a client-side route work.
//
// **The split is what makes an unknown route sayable.** Without it a mistyped
// API path got 200 and an HTML document — the SPA's index, served to a caller
// holding a JSON decoder — so "no such route" was not a thing the platform
// could express and every client had to infer it from a body it could not
// parse. `toft api` inferred something else entirely: the credential guard got
// there first and refused an unresolvable path as session-only, which reads as
// "your key is wrong" and sends an unattended run to fetch a human.
//
// The code is `route_not_found`, and it is the distinguisher a client needs.
// A 404 from a route that exists — an app id that names nothing — is the
// platform answering the question it was asked, and is not a usage error; this
// one is, and a client that can tell them apart can exit 2 for the second and
// 1 for the first.
//
// The path is quoted back rather than described. Whoever asked already knows
// what they sent, so it leaks nothing, and it is the whole of what they need
// to see the typo.
// methodNotAllowed is the 405 a route answers to a method it does not take,
// in the envelope every refusal carries. chi has already set Allow.
func (s *Server) methodNotAllowed(w http.ResponseWriter, r *http.Request) {
	// chi sets Allow only in its own default handler, so a custom one is told
	// nothing: the methods are found by asking the finished router, which is
	// what makes the answer able to name the verb that would have worked.
	allowed := s.allowedMethods(r.URL.Path)
	msg := r.URL.Path + " does not take " + r.Method
	hint := "check the method — nothing was attempted"
	if len(allowed) > 0 {
		list := strings.Join(allowed, ", ")
		w.Header().Set("Allow", list)
		msg += " — it takes " + list
		hint = "send " + strings.Join(allowed, " or ") + " instead — nothing was attempted"
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
		"error": msg,
		"code":  "method_not_allowed",
		"hint":  hint,
	})
}

// allowedMethods is every method the router answers for path, in a fixed
// order. Each probe takes a fresh route context: Match writes into the one it
// is given, and the request's own is still in use.
func (s *Server) allowedMethods(path string) []string {
	if s.mux == nil {
		return nil
	}
	var out []string
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if s.servesMethod(m, path) {
			out = append(out, m)
		}
	}
	return out
}

func (s *Server) notFound(spa http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			spa(w, r)
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "no route " + r.Method + " " + r.URL.Path + " on this install — " +
				"a path without a leading slash is read as relative to /api/v1/",
			"code": "route_not_found",
		})
	}
}
