package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"toft/internal/advisory"
	"toft/internal/deps"
	"toft/internal/entitlements"
)

// `toft advisory-import` — the advisory corpus for an install with no way
// out to the internet.
//
// **This is not an edge case; it is the customer this product is for.** An
// install put on-premises so its internal tools stay internal is
// disproportionately one with no outbound HTTPS at all, and a feature whose
// only input is a URL is a feature that customer cannot have.
//
// Everything downstream of this — matching, the sweep, staleness, the age on
// every finding — is identical to a sync, because the only thing that differed
// was transport: both call advisory.ReadSnapshot on the same archive shape.
//
// Two rules come with it, and both are about not letting the import lie:
//
//   - **The age is the snapshot's, never the import's.** A March corpus
//     imported in August is five months old and every surface says five
//     months. That is what --as-of is for, and why it is required rather than
//     defaulted to now.
//   - **Staleness is louder in this mode, not quieter**, since nothing will
//     refresh it. The threshold still applies and the admin page names this
//     command instead of offering a Sync button that cannot work.

const advisoryImportUsage = `Usage: toft advisory-import --ecosystem <name> --as-of <date> <file.zip>

Load an advisory snapshot from a file, for an install with no outbound HTTPS.
The archive is the same one the syncer fetches: OSV's per-ecosystem all.zip.

Flags:
  --ecosystem <name>  One of %s
  --as-of <date>      When the snapshot was published, as YYYY-MM-DD or an
                      RFC 3339 timestamp. Required, and it must be the
                      *snapshot's* date rather than today's: every surface
                      reports the corpus's age from it, so passing today would
                      make a five-month-old corpus read as fresh.

Reads DATABASE_URL from the environment or .env, like the server does. It
replaces that ecosystem's corpus in one transaction, so a failed import leaves
the previous one exactly where it was.
`

// runAdvisoryImport loads one ecosystem's snapshot from disk.
func runAdvisoryImport(args []string) error {
	fs := flag.NewFlagSet("advisory-import", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	ecosystem := fs.String("ecosystem", "", "which ecosystem this snapshot is for")
	asOf := fs.String("as-of", "", "when the snapshot was published")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || *ecosystem == "" || *asOf == "" {
		fmt.Printf(advisoryImportUsage, joinEcosystems())
		if fs.NArg() != 1 || *ecosystem == "" || *asOf == "" {
			return fmt.Errorf("an archive, --ecosystem and --as-of are all required")
		}
		return nil
	}

	published, err := parseAsOf(*asOf)
	if err != nil {
		return err
	}
	// Refused rather than silently accepted: a future date would make the
	// corpus read as fresh forever, which is the one thing this flag exists to
	// prevent.
	if published.After(time.Now().Add(24 * time.Hour)) {
		return fmt.Errorf("--as-of is in the future; it must be when the snapshot was published")
	}

	db, err := openDatabaseForCLI()
	if err != nil {
		return err
	}
	defer db.Close()

	// The same service the syncer uses, with a configuration that says the
	// feature is off for *fetching* purposes — an import makes no outbound
	// request and needs no endpoint or credential.
	svc := advisory.New(db, func() (advisory.Config, error) {
		return advisory.Config{}, nil
	})
	snap, err := svc.Import(fs.Arg(0), *ecosystem, published)
	if err != nil {
		return err
	}

	fmt.Printf("Imported %d advisories for %s, published %s (%s old).\n",
		len(snap.Records), *ecosystem, published.Format("2 January 2006"),
		roundedAge(time.Since(published)))
	if snap.Skipped > 0 {
		fmt.Printf("%d record(s) named no %s package and were not stored.\n", snap.Skipped, *ecosystem)
	}
	if snap.Unreadable > 0 {
		fmt.Printf("%d record(s) could not be read.\n", snap.Unreadable)
	}

	// Said out loud, because this is the mode where nothing will refresh it and
	// the age only goes one way from here.
	// With the install's own entitlements, as the server resolves it: the
	// perpetual source main.go wires. Without one the resolver falls back to the
	// unlicensed default, dependency policy reads as off, and Stale() is never
	// asked (docs/bugs 2026-10-22-e2e-identity-automation #2).
	policy, err := (&deps.Resolver{Store: db, Ents: entitlements.Perpetual{}}).Policy()
	if err == nil && policy.Stale() {
		fmt.Printf("\nThis corpus is already older than this install's staleness threshold, so " +
			"every finding will be shown with that warning beside it. Import a newer snapshot " +
			"to clear it.\n")
	}
	return nil
}

// parseAsOf accepts a plain date or a full timestamp. A date is what somebody
// reads off a release page; a timestamp is what a script has.
func parseAsOf(raw string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02", time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("--as-of %q is not a date: use YYYY-MM-DD or an RFC 3339 timestamp", raw)
}

func joinEcosystems() string {
	out := ""
	for i, e := range advisory.Synced {
		if i > 0 {
			out += ", "
		}
		out += e
	}
	return out
}

// roundedAge is the age in the units a person would say it in. The exact
// duration is not the point; "5 months" is.
func roundedAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return "under an hour"
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%d months", int(d.Hours()/24/30))
	}
}
