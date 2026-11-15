package gallery

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"toft/internal/database"
	"toft/internal/safefetch"
	"toft/internal/telemetry"
)

// syncInterval is how often the catalog is refreshed. Six hours because the
// catalog changes when the Toft team cuts a release, which is not often,
// and because an install polling a public repository every few minutes is
// traffic somebody eventually asks about.
const syncInterval = 6 * time.Hour

// manualSyncEvery bounds the administrator's Sync now button. One a minute is
// enough for "I just pushed a catalog change and want to see it" and not enough
// to be a way to hammer GitHub from a page.
const manualSyncEvery = time.Minute

// ErrSyncTooSoon is returned to an administrator who pressed Sync now twice.
var ErrSyncTooSoon = errors.New("the catalog was refreshed less than a minute ago")

// Service owns the cached catalog: it fetches, parses, caches and answers.
//
// The one behaviour worth stating up front is the one an on-premise customer
// cares about: while the mode is off this makes **no outbound request at all**.
// Not a request whose result is discarded — no request. A fresh install does
// not phone home, and every path through this file checks the mode first.
type Service struct {
	db         *database.DB
	fetcher    *Fetcher
	catalogURL string
	// Source is "environment" or "built-in", shown read-only in the admin panel
	// so an operator can see which one is in force without reading the process
	// environment.
	source          string
	platformVersion string

	mu           sync.RWMutex
	catalog      *Catalog
	lastAttempt  time.Time
	stop         chan struct{}
	stopped      bool
	syncInterval time.Duration
}

// New builds the service. It performs no I/O: the cached document is loaded by
// Start, and nothing is fetched until an administrator has turned the gallery
// on.
func New(db *database.DB, catalogURL, source, platformVersion string) *Service {
	return &Service{
		db:              db,
		fetcher:         NewFetcher(catalogURL),
		catalogURL:      catalogURL,
		source:          source,
		platformVersion: platformVersion,
		stop:            make(chan struct{}),
		syncInterval:    syncInterval,
	}
}

// NewForTesting is New with the fetcher's transport replaced, so a test can
// point the service at an httptest server whose certificate nothing trusts and
// whose address is on loopback. Everything else — the parser, the mode check,
// the caching, the rate limit — is the production path.
//
// The address guard is what a test server cannot survive, and it is the one
// rule this constructor gives up. It has its own tests in fetch_test.go.
func NewForTesting(db *database.DB, catalogURL, source, platformVersion string, transport http.RoundTripper) *Service {
	s := New(db, catalogURL, source, platformVersion)
	s.fetcher = &Fetcher{client: safefetch.NewWithTransport(catalogURL, transport, fetchTimeout)}
	return s
}

// Fetcher is the guarded HTTP client, for the one caller outside this package
// that needs it: staging an example's archive. Exposed rather than duplicated
// so that the size limit, the redirect cap and the refusal to reach this
// network apply to every fetch the catalog can provoke, not only to the
// catalog document itself.
func (s *Service) Fetcher() *Fetcher { return s.fetcher }

// CatalogURL is the effective URL, and Source says where it came from. Both are
// read-only everywhere: no API writes them, and the admin panel shows them
// rather than offering them as a field. Where the catalog lives is a fact about
// how the install was built, not a decision an administrator revisits.
func (s *Service) CatalogURL() string { return s.catalogURL }
func (s *Service) Source() string     { return s.source }

// Mode reads the administrator's setting. Read fresh rather than cached: off
// must mean off *now*, including for a sync already scheduled.
func (s *Service) Mode() string {
	if s == nil || s.db == nil {
		return database.GalleryOff
	}
	settings, err := s.db.GetPlatformSettings()
	if err != nil {
		// Fail closed, as the capability switches do: a settings read that
		// fails must not accidentally offer something nobody turned on.
		log.Printf("gallery: reading platform settings, treating the gallery as off: %v", err)
		return database.GalleryOff
	}
	return settings.GalleryMode
}

// Enabled reports whether the gallery is offered at all.
func (s *Service) Enabled() bool { return s.Mode() != database.GalleryOff }

// Start loads the cached document and begins refreshing.
//
// The cache is loaded whatever the mode, because parsing a document already on
// disk is not a network call and an install that starts offline should still
// have its gallery the moment an administrator switches it on.
func (s *Service) Start() {
	s.loadCached()
	go s.loop()
}

// Stop ends the refresh loop. Safe to call twice.
func (s *Service) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		close(s.stop)
	}
}

func (s *Service) loop() {
	// A short delay before the first sync so a boot that is already doing
	// migrations, reconciliation and health checks is not also waiting on a
	// third-party HTTP request.
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-timer.C:
			if s.Enabled() {
				if err := s.Sync(context.Background()); err != nil {
					log.Printf("gallery: sync: %v", err)
				}
			}
			timer.Reset(s.syncInterval)
		}
	}
}

// loadCached parses whatever the last successful fetch stored. A document that
// no longer parses — this binary is older than the one that cached it — leaves
// the in-memory catalog empty and says so, rather than crashing a boot over a
// gallery.
func (s *Service) loadCached() {
	cache, err := s.db.GetGalleryCatalog()
	if err != nil {
		log.Printf("gallery: reading the cached catalog: %v", err)
		return
	}
	if strings.TrimSpace(cache.Document) == "" {
		return
	}
	doc, err := Parse([]byte(cache.Document))
	if err != nil {
		log.Printf("gallery: the cached catalog no longer parses: %v", err)
		return
	}
	s.mu.Lock()
	s.catalog = doc
	s.mu.Unlock()
}

// Sync refreshes the catalog. It is a no-op — and, crucially, makes no request
// — while the gallery is off.
func (s *Service) Sync(ctx context.Context) error {
	if !s.Enabled() {
		// Off means no outbound request, and it also means no series: a fresh
		// install that has never contacted the catalog should not carry a
		// gallery metric at all, for the same reason it carries no gallery
		// routes (ADR §28).
		return nil
	}

	ctx, span := telemetry.StartClient(ctx, "gallery.sync")
	start := time.Now()
	var syncErr error
	defer func() {
		span.End(syncErr)
		telemetry.RecordGallerySync(ctx, telemetry.Outcome(syncErr), time.Since(start))
	}()

	s.mu.Lock()
	s.lastAttempt = time.Now()
	s.mu.Unlock()

	cache, err := s.db.GetGalleryCatalog()
	if err != nil {
		syncErr = err
		return err
	}

	res, err := s.fetcher.FetchCatalog(ctx, s.catalogURL, cache.ETag)
	if err != nil {
		// The document is deliberately left alone: the last good catalog keeps
		// serving with its age beside it, because an empty gallery and an
		// unreachable one must stay tellable apart.
		//
		// The *metric* records the fetch failing even though the function
		// returns whatever SaveGalleryCatalogFailure did: what an operator wants
		// to know is that this install cannot reach the catalog, not that a row
		// was written about it successfully.
		syncErr = err
		return s.db.SaveGalleryCatalogFailure(database.GallerySyncUnreachable, err.Error())
	}
	if res.NotModified {
		return s.db.TouchGalleryCatalog()
	}

	doc, err := Parse(res.Body)
	if err != nil {
		status := database.GallerySyncUnparseable
		if errors.Is(err, ErrCatalogTooNew) {
			// Not a failure to report as breakage: the catalog is fine and this
			// install is behind. The admin panel says so in those words.
			status = database.GallerySyncTooNew
		}
		syncErr = err
		return s.db.SaveGalleryCatalogFailure(status, err.Error())
	}

	s.mu.Lock()
	s.catalog = doc
	s.mu.Unlock()

	if len(doc.Dropped) > 0 {
		for _, d := range doc.Dropped {
			log.Printf("gallery: dropped item %q (%s): %s", d.Name, d.ID, d.Reason)
		}
	}
	return s.db.SaveGalleryCatalog(string(res.Body), res.ETag, len(doc.Items), len(doc.Dropped))
}

// SyncNow is the administrator's button, rate-limited so that a page cannot
// become a way to hammer the catalog host.
func (s *Service) SyncNow(ctx context.Context) error {
	s.mu.RLock()
	last := s.lastAttempt
	s.mu.RUnlock()
	if !last.IsZero() && time.Since(last) < manualSyncEvery {
		return ErrSyncTooSoon
	}
	return s.Sync(ctx)
}

// Catalog returns the parsed catalog, or nil when nothing has been cached yet.
func (s *Service) Catalog() *Catalog {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.catalog
}

// Item looks one entry up by its id. The second return distinguishes "no such
// item" from "the item is retired", which the caller must keep apart: a retired
// item still has apps installed from it, and those apps must be told they are
// no longer listed rather than left implying they are current.
func (s *Service) Item(id string) (Item, bool) {
	return s.Catalog().Item(id)
}

// ItemsFor returns what this viewer may see, in catalog order.
//
// The filtering is here rather than in the UI because a UI filter is not an
// access control: extensions are administrator-only, and under
// extensions_only the examples are not listed at all.
func (s *Service) ItemsFor(mode string, admin bool) []Item {
	c := s.Catalog()
	if c == nil || mode == database.GalleryOff {
		return nil
	}
	out := make([]Item, 0, len(c.Items))
	for _, it := range c.Items {
		switch it.Kind {
		case KindExtension:
			if !admin {
				continue
			}
		case KindExample:
			if mode != database.GalleryAll {
				continue
			}
		}
		out = append(out, it)
	}
	return out
}

// PlatformVersion is the version items are checked against.
func (s *Service) PlatformVersion() string { return s.platformVersion }

// Deployable answers whether one item may be deployed on this install right
// now, and why not when it may not. Every refusal names its own reason: a
// blocked Deploy button with no sentence beside it is a support call.
func (s *Service) Deployable(it Item, mode string, admin bool) (bool, string) {
	switch mode {
	case database.GalleryOff:
		return false, "The gallery is switched off on this install."
	case database.GalleryExtensionsOnly:
		if it.Kind == KindExample {
			return false, "Deploying examples is switched off on this install."
		}
	}
	if it.Kind == KindExtension && !admin {
		return false, "Extensions are deployed by administrators."
	}
	if ok, need := it.SatisfiesPlatform(s.platformVersion); !ok {
		return false, "This needs Toft " + need + " or newer; this install is " + s.platformVersion + "."
	}
	return true, ""
}
