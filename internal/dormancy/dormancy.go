// Package dormancy locks accounts nobody has used for the administrator's
// threshold, and reports the ones it will not lock.
//
// Why this exists at all, on a product whose identity is federated: Toft
// has no SCIM and no deprovisioning webhook, so nothing tells it when the
// customer's identity provider deletes somebody. The `users` row is a shadow
// that never learns its original died — and it still carries app ownership,
// group memberships, access grants, and personal API keys that authenticate
// with no round trip to the provider at all. This sweep is the only backstop
// this install has for that, which is also why it is off by default: it is a
// backstop, not a policy every install wants.
//
// See ADR §48 and docs/invariants.md § Identity.
package dormancy

import (
	"context"
	"log"
	"sync"
	"time"

	"toft/internal/database"
	"toft/internal/telemetry"
)

// sweepInterval is how often the sweep looks.
//
// An hour, not a minute: the threshold is measured in days, so a finer grain
// buys nothing an administrator could perceive and costs a query per tick
// forever. It is fine enough that switching the feature off takes effect within
// the hour, which for a control that locks accounts is the direction that
// matters — the switch stops it, and nothing it has already done is undone by
// the sweep.
const sweepInterval = time.Hour

// Store is the database surface the sweep needs, named here rather than taken
// as *database.DB so the queries it depends on are visible in one place and a
// test can substitute one.
type Store interface {
	GetPlatformSettings() (database.PlatformSettings, error)
	ListDormantUsers(cutoff time.Time) ([]database.UserActivity, error)
	LockDormantUser(u database.UserActivity, thresholdDays int, now time.Time) (bool, error)
}

// Sweeper locks dormant accounts on a timer.
type Sweeper struct {
	store Store

	// now is swappable so a test drives time rather than sleeping through it.
	now func() time.Time

	stop chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

func New(store Store) *Sweeper {
	return &Sweeper{
		store: store,
		now:   func() time.Time { return time.Now().UTC() },
		stop:  make(chan struct{}),
	}
}

// Start runs the sweep loop for the life of the process.
func (s *Sweeper) Start() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(sweepInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				s.Tick()
			case <-s.stop:
				return
			}
		}
	}()
}

func (s *Sweeper) Close() {
	s.once.Do(func() { close(s.stop) })
	s.wg.Wait()
}

// Result is what one pass did. Returned for the tests and the log line; nothing
// downstream branches on it.
type Result struct {
	// Locked is how many accounts this pass locked.
	Locked int
	// DormantAdmins is how many administrators were past the threshold and
	// deliberately left alone.
	DormantAdmins int
	// AlreadyLocked is how many dormant accounts were already locked.
	AlreadyLocked int
	// NotActivated is how many accounts past the threshold are still waiting
	// for an administrator to activate them, and were left alone. Reported for
	// the same reason DormantAdmins is: a pass that quietly skipped rows would
	// leave the two figures in the log unable to account for the candidates.
	NotActivated int
}

// Tick locks everything eligible, once. Exported so a test can drive one pass
// without a ticker.
func (s *Sweeper) Tick() Result {
	ctx, span := telemetry.Start(context.Background(), "dormancy.sweep")
	start := time.Now()
	outcome := telemetry.OutcomeFailed
	var res Result
	defer func() {
		span.EndOK(outcome)
		telemetry.RecordSweep(ctx, telemetry.SweepDormancy, outcome, time.Since(start))
	}()

	// Read per tick, never cached at boot: an administrator turning this off
	// means "stop locking people now", and a sweep that kept going afterwards
	// would make the switch a lie. Same rule the idle reaper follows.
	settings, err := s.store.GetPlatformSettings()
	if err != nil {
		// Fail closed, and here that word is load-bearing in a way it is not
		// for the idle reaper: a settings read that fails must never fall back
		// to a default threshold, because the default is a *policy* and
		// guessing it locks real people out of the platform. No settings, no
		// sweep.
		log.Printf("dormancy: reading platform settings, skipping sweep: %v", err)
		return res
	}
	if !settings.DormantLockEnabled {
		// Off is not a failure. A sweep that ran and found the feature disabled
		// did exactly what it was supposed to.
		outcome = telemetry.OutcomeSkipped
		return res
	}

	now := s.now()
	cutoff := now.Add(-time.Duration(settings.DormantLockDays) * 24 * time.Hour)

	candidates, err := s.store.ListDormantUsers(cutoff)
	if err != nil {
		log.Printf("dormancy: listing dormant accounts: %v", err)
		return res
	}

	for _, u := range candidates {
		// Already locked, by an administrator or by an earlier pass. Counted
		// rather than skipped silently, because "twelve dormant accounts, none
		// locked this pass" and "twelve dormant accounts, none found" are
		// different things to read in a log at three in the morning.
		if u.Locked() {
			res.AlreadyLocked++
			continue
		}

		// **Administrators are never locked by this sweep**, and this is not a
		// setting. Three reasons, each sufficient on its own:
		//
		//   - The built-in local administrator exists to bootstrap the install
		//     and to recover it. On an install that federated on day one and
		//     never signed in locally again, it is dormant *by design* — and it
		//     is the account you need on the day the identity provider breaks.
		//     Locking it turns a routine outage into the machine-access
		//     recovery path.
		//   - Small installs have two or three administrators who use the
		//     platform through their own apps rather than the admin console.
		//     Locking them leaves an unadministrable system with nobody left to
		//     unlock it.
		//   - handleLockUser already refuses to lock the last unlocked admin by
		//     hand. A timer that could do what the button refuses to do would be
		//     the same protection with a hole in it.
		//
		// The compliance case an auditor cares most about — a departed
		// administrator — is met by *reporting* rather than by acting: a lock
		// here is a decision with a name on it, and this sweep has no name.
		// An account nobody has activated has never been able to sign in, so
		// its clock has only ever read `created_at` and it crosses any
		// threshold by simply waiting. Locking it would answer a queue with a
		// second refusal — the administrator would find "locked" where they
		// left "pending", and activating it would then take an unlock as well.
		// The queue is the place this account is dealt with.
		if u.PendingActivation() {
			res.NotActivated++
			continue
		}

		if u.SystemRole == database.SystemRoleAdmin {
			res.DormantAdmins++
			log.Printf("dormancy: %s (administrator) has been idle for %d day(s) — not locked; "+
				"administrators are never locked automatically, review this account by hand",
				u.Login, int(u.DormantFor(now).Hours()/24))
			continue
		}

		locked, err := s.store.LockDormantUser(u, settings.DormantLockDays, now)
		if err != nil {
			// One account's failure is not the sweep's. The next pass is an
			// hour away and will find it again.
			log.Printf("dormancy: locking %s: %v", u.Login, err)
			continue
		}
		if !locked {
			// Somebody locked it between the list and the update.
			res.AlreadyLocked++
			continue
		}
		res.Locked++
		log.Printf("dormancy: locked %s after %d day(s) idle (threshold %d)",
			u.Login, int(u.DormantFor(now).Hours()/24), settings.DormantLockDays)
	}

	if res.DormantAdmins > 0 {
		log.Printf("dormancy: %d administrator account(s) past the threshold and left alone", res.DormantAdmins)
	}
	if res.NotActivated > 0 {
		log.Printf("dormancy: %d account(s) past the threshold are still awaiting activation and were left alone", res.NotActivated)
	}
	outcome = telemetry.OutcomeSucceeded
	return res
}
