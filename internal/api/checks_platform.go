package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"toft/internal/advisory"
	"toft/internal/database"
)

// The platform's own moving parts: the advisory corpus, what the outside world
// last said about the front door, and whether this process and its database
// agree about what time it is.

// checkCorpus is the counterweight to a deliberate design.
//
// A stale advisory feed never blocks a deploy — that is the right call, and it
// is exactly why the staleness has to be visible somewhere a person reads.
// Otherwise the install goes on answering "no known vulnerabilities" from a
// snapshot a month old, in the same words it uses when the snapshot is an hour
// old, and nothing anywhere distinguishes the two.
//
// The age is the oldest **covered** ecosystem's, which is OldestFeedAge's rule
// and not a second one: an ecosystem whose silence is not a verdict cannot make
// a verdict stale, and CRAN — republished about twice a year — would otherwise
// hold this row at warn forever and drown the reading it exists to give.
//
// `skip` when the policy is off, which includes an install that is not licensed
// for it: a corpus nothing consults is not a finding about anything. A lapsed
// entitlement inside its grace period still reads as on, so this check keeps
// running through a late renewal — a license never takes down running code, and
// it must not quietly stop reporting on it either.
func (s *Server) checkCorpus(_ context.Context, rp *checkReport) {
	policy, err := s.deps.Policy()
	if err != nil {
		rp.add("advisory_corpus", "Advisory corpus", checkWarn, dbReadFailed("advisory_corpus", "the dependency policy", err))
		return
	}
	if policy.Off() {
		rp.add("advisory_corpus", "Advisory corpus", checkSkip,
			"The dependency policy is off, so nothing is checked against the corpus and its age decides nothing.")
		return
	}

	oldest, err := s.db.OldestFeedAge()
	if err != nil {
		rp.add("advisory_corpus", "Advisory corpus", checkWarn, dbReadFailed("advisory_corpus", "the feed ages", err))
		return
	}
	if oldest == nil {
		// The one that matters most and is easiest to miss: the policy has been
		// on since somebody switched it on, and every verdict it has ever given
		// was taken against an empty corpus.
		rp.add("advisory_corpus", "Advisory corpus", checkWarn,
			"The dependency policy is on and no covered feed has ever synced, so every verdict was "+
				"taken against an empty corpus.")
		return
	}

	// One loaded corpus made the oldest-age figure true for all of them, so a
	// covered ecosystem that has never loaded is named on its own: every
	// release in it reads "not scanned, no corpus" and nothing else says why
	// (security review 2026-11-14, DG-5).
	loaded, err := s.db.LoadedEcosystems()
	if err != nil {
		rp.add("advisory_corpus", "Advisory corpus", checkWarn, dbReadFailed("advisory_corpus", "the feed states", err))
		return
	}
	var never []string
	for _, eco := range advisory.Ecosystems {
		if !loaded[eco] {
			never = append(never, eco)
		}
	}
	if len(never) > 0 {
		rp.add("advisory_corpus", "Advisory corpus", checkWarn,
			fmt.Sprintf("No advisory data has ever loaded for %s, so releases using it are listed and "+
				"checked against nothing. Sync or import it.", strings.Join(never, ", ")))
		return
	}

	age := time.Since(*oldest)
	total, malware, err := s.db.CountAdvisories()
	if err != nil {
		rp.add("advisory_corpus", "Advisory corpus", checkWarn, dbReadFailed("advisory_corpus", "the advisory counts", err))
		return
	}
	if age > corpusStaleAfter {
		rp.add("advisory_corpus", "Advisory corpus", checkWarn,
			fmt.Sprintf("The oldest covered feed last synced %s ago, so its sync has stopped. A stale "+
				"corpus never blocks a deploy, so this is the only place it shows.",
				humanDuration(age)))
		return
	}
	rp.add("advisory_corpus", "Advisory corpus", checkPass,
		fmt.Sprintf("%s advisories (%s malware), oldest covered feed synced %s ago.",
			thousands(total), thousands(malware), humanDuration(age)))
}

// checkClock compares this process's clock with the database's.
//
// Not an NTP check — nothing here dials a time server, on the same rule as every
// other check. The database is the clock every other node in this install
// already agrees with, and disagreeing with it is what breaks session ceilings,
// cron claims, retry windows and token expiry. Each of those presents as
// something else entirely, which is why one row naming the cause is worth more
// than the four that would otherwise be filed against it.
//
// The check cannot bootstrap itself out of a clock broken badly enough to stop
// the query: if the read fails the row says so, which is the honest answer.
func (s *Server) checkClock(ctx context.Context, rp *checkReport) {
	var dbNow time.Time
	if err := s.db.Conn().SQL().QueryRowContext(ctx, "SELECT now()").Scan(&dbNow); err != nil {
		rp.add("clock", "Clock", checkWarn, dbReadFailed("clock", "the clock", err))
		return
	}

	status, detail := clockVerdict(time.Since(dbNow))
	rp.add("clock", "Clock", status, detail)
}

// clockVerdict is the whole judgement, separated from the read so that both
// directions can be tested without a database that can be told to lie about
// what time it is. A platform ahead of its database expires sessions early and
// one behind it honours them late; neither is the benign one, so the verdict is
// on the magnitude and the direction is only ever words.
func clockVerdict(skew time.Duration) (status, detail string) {
	direction := "ahead of"
	if skew < 0 {
		skew, direction = -skew, "behind"
	}
	switch {
	case skew >= clockSkewFail:
		return checkFail, fmt.Sprintf(
			"This platform's clock is %s %s the database's, which skews sessions, schedules and token expiry.",
			humanSkew(skew), direction)
	case skew >= clockSkewWarn:
		return checkWarn, fmt.Sprintf("This platform's clock is %s %s the database's.",
			humanSkew(skew), direction)
	default:
		return checkPass, fmt.Sprintf("Within %s of the database's clock.", humanSkew(clockSkewWarn))
	}
}

func humanSkew(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return d.Round(time.Second).String()
}

// checkTelemetry moves an existing warning onto the page it belongs on.
//
// The sentence is already written, in the place that knows the condition, and
// it is used verbatim: rewording it here would make two spellings of one
// finding, and the one somebody greps for would be whichever they saw last.
//
// Nothing here reaches the collector. Telemetry is exported, never resident and
// never load-bearing, and a check that dialled the OTLP endpoint would let an
// optional capability slow down a diagnostic.
func (s *Server) checkTelemetry(_ context.Context, rp *checkReport) {
	t := s.cfg.Telemetry
	if !t.Enabled() && !t.ScrapeEnabled() {
		rp.add("telemetry", "Telemetry", checkSkip,
			"Neither export nor the scrape endpoint is configured. Absent, not dormant.")
		return
	}
	if warning := t.MetricsBindWarning(); warning != "" {
		rp.add("telemetry", "Telemetry", checkWarn, warning)
		return
	}

	var on []string
	if t.Enabled() {
		on = append(on, "export")
	}
	if t.ScrapeEnabled() {
		on = append(on, "the scrape endpoint")
	}
	rp.add("telemetry", "Telemetry", checkPass, sentence([]string{strings.Join(on, " and ") + " configured"}))
}

// recentWaits is the warning's tail when the pool has queued requests for more
// than a second in total recently, at poolSlowWait or more each on average, and
// "" when it has not. span is how far back the counters reach
// (poolWatch.recent); zero means since the platform started.
//
// The average is what a person feels. A drills fan-out queued 230 times for
// 12 ms each and warned for ten minutes that every page was slow, which no page
// was; a pool that is really too small makes each request wait for a whole
// query ahead of it, and that is tens of milliseconds and up.
func recentWaits(count int64, wait, span time.Duration) string {
	if count <= 0 || wait <= time.Second || wait/time.Duration(count) < poolSlowWait {
		return ""
	}
	when := "since the platform started"
	if m := int(span.Round(time.Minute) / time.Minute); span > 0 && m <= 1 {
		when = "in the last minute"
	} else if span > 0 {
		when = fmt.Sprintf("in the last %d minutes", m)
	}
	return fmt.Sprintf("%s across %d waits %s.", wait.Round(time.Millisecond), count, when)
}

// checkPool reads counters this process already holds. No query.
//
// This is the failure that makes every page slow and produces no error anywhere
// to look up: requests queue on a connection rather than fail, so the symptom is
// latency and the cause is invisible. It is also one of the few things on this
// list that host metrics genuinely cannot show — the pool is inside this
// process, and from outside it looks like a busy database.
func (s *Server) checkPool(_ context.Context, rp *checkReport) {
	stats := s.db.Conn().SQL().Stats()
	if stats.MaxOpenConnections <= 0 {
		rp.add("db_pool", "Database pool", checkSkip, "The pool is not bounded, so it cannot saturate.")
		return
	}

	used := float64(stats.InUse) / float64(stats.MaxOpenConnections)
	waited := recentWaits(s.pool.recent(time.Now(), stats))
	switch {
	case used >= poolSaturation:
		rp.add("db_pool", "Database pool", checkWarn,
			fmt.Sprintf("%d of %d connections in use. Requests now wait for one, so the install slows down without an error.",
				stats.InUse, stats.MaxOpenConnections))
	case waited != "":
		rp.add("db_pool", "Database pool", checkWarn,
			fmt.Sprintf("%d of %d connections in use, and requests have waited %s",
				stats.InUse, stats.MaxOpenConnections, waited))
	default:
		rp.add("db_pool", "Database pool", checkPass,
			fmt.Sprintf("%d of %d connections in use.", stats.InUse, stats.MaxOpenConnections))
	}
}

// checkMigrations is a tripwire, and it is worth being precise about what it can
// actually catch — the PRD's three scenarios were wrong and the code says so.
//
// database.New applies migrations at boot and **closes the pool and refuses to
// start if it cannot**, with no switch to skip them. So the cases that sound
// like this row's subject are all boot failures rather than findings: a role
// that cannot DDL never opens the database, and an older binary serving beside
// a newer one carries *fewer* files, which goose reports as untracked and this
// does not count.
//
// What is left is narrow and real: the database moved underneath a process that
// is already running — a restore from a backup taken before the migrations, or
// a pool repointed at another database. Rare, one indexed query, and the kind of
// thing that otherwise surfaces as a column not existing three features away.
func (s *Server) checkMigrations(ctx context.Context, rp *checkReport) {
	pending, err := database.PendingMigrations(ctx, s.db.Conn().SQL())
	if err != nil {
		rp.add("migrations", "Schema migrations", checkWarn, dbReadFailed("migrations", "the applied migrations", err))
		return
	}
	if pending > 0 {
		rp.add("migrations", "Schema migrations", checkWarn,
			fmt.Sprintf("%d migration(s) this binary carries are not applied, most likely because the "+
				"database was restored from an older backup while the platform ran. Restart the platform to apply them.", pending))
		return
	}
	rp.add("migrations", "Schema migrations", checkPass, "The schema is up to date with this binary.")
}
