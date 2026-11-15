package advisory

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// The refresh loop, in the gallery syncer's shape and for the gallery syncer's
// reasons: a timer, a rate-limited SyncNow for the administrator's button, a
// recorded last-success and last-error, and — the load-bearing one — a failed
// sync that leaves the last good corpus exactly where it was.
//
// **Off means no outbound request at all.** Not a request whose result is
// discarded: no request. A fresh install does not contact osv.dev because
// nobody asked it to, and every path through this file asks the configuration
// first.

// ErrSyncTooSoon is returned to an administrator who pressed Sync now twice.
var ErrSyncTooSoon = errors.New("the advisory feed was refreshed less than a minute ago")

// manualSyncEvery bounds the administrator's button, so that a page cannot
// become a way to hammer the feed's host.
const manualSyncEvery = time.Minute

// FeedState is one ecosystem's corpus as the platform last left it.
type FeedState struct {
	Ecosystem     string     `json:"ecosystem"`
	Endpoint      string     `json:"endpoint,omitempty"`
	LastSyncAt    *time.Time `json:"last_sync_at,omitempty"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	SnapshotID    string     `json:"snapshot_id,omitempty"`
	ETag          string     `json:"-"`
	AdvisoryCount int        `json:"advisory_count"`
	// Origin is "sync" or "import". An air-gapped install carries a snapshot
	// in by hand, and the admin page has to name the command rather than offer
	// a Sync button that cannot work.
	Origin string `json:"origin"`
	// SnapshotAt is **the snapshot's own age**, never when it was loaded. A
	// March corpus imported in August is five months old and every surface
	// says five months, or the import becomes a way to make a stale feed look
	// fresh.
	SnapshotAt *time.Time `json:"snapshot_at,omitempty"`
	// Covered is Covered(Ecosystem), carried on the row rather than worked out
	// again by a reader. It is what makes the age above readable: an uncovered
	// corpus is months behind by its upstream's cadence rather than by any
	// failure here, it is deliberately left out of OldestFeedAge, and a page
	// showing its date beside three fresh ones without saying that is showing
	// a number nobody can interpret.
	Covered bool `json:"covered"`
}

// Age is how old this corpus is, and whether that is knowable at all.
func (s FeedState) Age() (time.Duration, bool) {
	if s.SnapshotAt == nil {
		return 0, false
	}
	return time.Since(*s.SnapshotAt), true
}

// Store is the slice of the database this package writes through.
//
// An interface rather than a *database.DB, following the events.Recorder
// precedent, and here it is load-bearing rather than stylistic: `database`
// imports this package for its types, so an import the other way would close a
// cycle.
type Store interface {
	// ReplaceSnapshot swaps one ecosystem's corpus in a single transaction, so
	// a failed sync leaves the last good one intact.
	ReplaceSnapshot(ecosystem, snapshotID string, records []Record, state FeedState) error
	// RecordSyncFailure updates the state without touching the corpus.
	RecordSyncFailure(ecosystem, endpoint, message string) error
	// FeedStates is every ecosystem's state, for the age checks and the admin
	// page.
	FeedStates() ([]FeedState, error)
}

// Config is what the service needs to know about the install's decision,
// resolved fresh on every tick.
//
// A struct read through a function rather than fields on the service, because
// **off must mean off now**: an administrator switching the policy off during
// a six-hour interval must not get one more fetch out of a decision the
// service cached at construction.
type Config struct {
	// Enabled is the whole gate: licensed, and the policy is not off. False
	// means no request.
	Enabled  bool
	Endpoint string
	// Credential is the decrypted mirror credential, empty for a public feed.
	// It never reaches a workload — runtime.BaseEnv() is an allow-list — and
	// it is never logged.
	Credential string
	// Ecosystems is which corpora to keep. Narrowing it to what the install
	// actually deploys is the difference between one archive and three.
	Ecosystems []string
	Interval   time.Duration
	// ScratchDir is where a snapshot is spooled while it is read: APPS_DIR's
	// staging directory, on the disk the releases are on. Empty means TMPDIR.
	ScratchDir string
}

// ConfigSource resolves the configuration. It returns an error rather than a
// zero Config on failure so that a settings read that fails is a logged
// no-request rather than a silent switch-off that looks identical to the
// administrator having turned it off.
type ConfigSource func() (Config, error)

// Service owns the corpus: it fetches, parses, swaps and reports.
type Service struct {
	store  Store
	config ConfigSource

	mu          sync.Mutex
	lastAttempt time.Time
	stop        chan struct{}
	stopped     bool
	// syncing serializes the loop against the administrator's button, so two
	// full-corpus swaps cannot interleave.
	syncing sync.Mutex
}

// New builds the service. It performs no I/O and starts no goroutine.
func New(store Store, config ConfigSource) *Service {
	return &Service{store: store, config: config, stop: make(chan struct{})}
}

// Start begins refreshing.
func (s *Service) Start() { go s.loop() }

// Stop ends the refresh loop. Safe to call twice.
func (s *Service) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		close(s.stop)
	}
}

func (s *Service) loop() {
	// A short delay before the first sync so a boot already doing migrations,
	// reconciliation and a build-queue sweep is not also waiting on a
	// third-party download of tens of megabytes.
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-timer.C:
			next := 6 * time.Hour
			cfg, err := s.config()
			switch {
			case err != nil:
				log.Printf("advisory: reading the feed configuration, skipping this refresh: %v", err)
			case !cfg.Enabled:
				// Off. No request, and no log line either — a fresh install
				// with the feature off should not narrate a decision nobody
				// made every six hours.
			default:
				if cfg.Interval > 0 {
					next = cfg.Interval
				}
				if err := s.sync(context.Background(), cfg); err != nil {
					log.Printf("advisory: sync: %v", err)
				}
			}
			timer.Reset(next)
		}
	}
}

// SyncNow is the administrator's button, rate-limited on the gallery syncer's
// pattern.
func (s *Service) SyncNow(ctx context.Context) error {
	if s == nil {
		return errors.New("the advisory feed is not configured")
	}
	s.mu.Lock()
	last := s.lastAttempt
	s.mu.Unlock()
	if !last.IsZero() && time.Since(last) < manualSyncEvery {
		return ErrSyncTooSoon
	}

	cfg, err := s.config()
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		// Said rather than silently succeeded: an administrator who pressed a
		// button is owed the reason nothing happened.
		return errors.New("dependency policy is off, so the advisory feed is not refreshed")
	}
	return s.sync(ctx, cfg)
}

// sync refreshes every configured ecosystem.
//
// One ecosystem failing does not stop the others, and each is swapped
// independently: npm succeeding while PyPI 404s must leave npm current and
// PyPI stale, which is exactly why the feed state is a row per ecosystem
// rather than one row for the feed.
func (s *Service) sync(ctx context.Context, cfg Config) error {
	s.syncing.Lock()
	defer s.syncing.Unlock()

	s.mu.Lock()
	s.lastAttempt = time.Now()
	s.mu.Unlock()

	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	fetcher := NewFetcher(endpoint, cfg.Credential)
	fetcher.scratch = cfg.ScratchDir

	states, err := s.store.FeedStates()
	if err != nil {
		return err
	}
	etags := map[string]string{}
	for _, st := range states {
		etags[st.Ecosystem] = st.ETag
	}

	var failures []string
	for _, ecosystem := range cfg.Ecosystems {
		if !KnownEcosystem(ecosystem) {
			continue
		}
		if err := s.syncOne(ctx, fetcher, endpoint, ecosystem, etags[ecosystem]); err != nil {
			// The corpus is deliberately left alone. The last good one keeps
			// serving with its age beside it, because a corpus that is empty
			// and one that is unreachable must stay tellable apart.
			if recErr := s.store.RecordSyncFailure(ecosystem, endpoint, err.Error()); recErr != nil {
				log.Printf("advisory: recording the %s sync failure: %v", ecosystem, recErr)
			}
			failures = append(failures, fmt.Sprintf("%s: %v", ecosystem, err))
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

func (s *Service) syncOne(ctx context.Context, f *Fetcher, endpoint, ecosystem, etag string) error {
	snap, err := f.Fetch(ctx, endpoint, ecosystem, etag)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if snap.NotModified {
		// Still current. Only the "we checked" clock moves; the snapshot's own
		// age deliberately does not, so an archive unchanged for a month reads
		// as a month old rather than as fresh.
		return s.store.ReplaceSnapshot(ecosystem, "", nil, FeedState{
			Ecosystem: ecosystem, Endpoint: endpoint,
			LastSyncAt: &now, LastSuccessAt: &now, Origin: "sync", ETag: etag,
		})
	}

	at := snap.At
	return s.store.ReplaceSnapshot(ecosystem, uuid.New().String(), snap.Records, FeedState{
		Ecosystem:     ecosystem,
		Endpoint:      endpoint,
		LastSyncAt:    &now,
		LastSuccessAt: &now,
		ETag:          snap.ETag,
		AdvisoryCount: len(snap.Records),
		Origin:        "sync",
		SnapshotAt:    &at,
	})
}

// Import loads a snapshot somebody carried in, for an install with no outbound
// HTTPS at all.
//
// This is not an edge case — it is the customer this product is for. An
// install put on-premises so its internal tools stay internal is
// disproportionately one with no egress, and a feature whose only input is a
// URL is a feature that customer cannot have. Everything downstream —
// matching, the sweep, staleness, the age on every finding — is identical,
// because the only thing that differed was transport.
//
// `at` is **the snapshot's own date**, supplied by the caller from the file
// rather than taken as now. A March corpus imported in August is five months
// old and must read as five months old.
func (s *Service) Import(path, ecosystem string, at time.Time) (Snapshot, error) {
	if !KnownEcosystem(ecosystem) {
		return Snapshot{}, fmt.Errorf("%q is not an ecosystem this build resolves (%s)",
			ecosystem, strings.Join(Synced, ", "))
	}
	snap, err := ReadSnapshot(path, ecosystem, "import")
	if err != nil {
		return Snapshot{}, err
	}
	now := time.Now().UTC()
	at = at.UTC()
	if err := s.store.ReplaceSnapshot(ecosystem, uuid.New().String(), snap.Records, FeedState{
		Ecosystem:     ecosystem,
		Endpoint:      "",
		LastSyncAt:    &now,
		LastSuccessAt: &now,
		AdvisoryCount: len(snap.Records),
		Origin:        "import",
		SnapshotAt:    &at,
	}); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}
