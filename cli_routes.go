package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"toft/internal/api"
)

// `toft routes` — what this binary serves, asked of the router itself.
//
// It exists for scripts/zap-scan.sh, which builds an authenticated crawl from
// it: a security scanner only attacks requests it has already seen, so the
// crawl *is* the coverage, and a crawl assembled from a hand-written list of
// URLs silently stops testing every route added after somebody last edited the
// list. Asking the router removes that failure mode entirely.
//
// Like local-admin and advisory-import it needs no database — but for a
// different reason. Those talk to Postgres and not to the server; this one
// talks to neither. Building a chi router is pure, so this is a question the
// binary can answer about itself with nothing else running.

const routesUsage = `Usage: toft routes [flags]

Print every route this binary serves, on both listeners: the platform origin
(the API and the SPA) and the apps gateway. Reads no configuration and opens no
database.

Flags:
  --json            Emit JSON — {"method","pattern","router"} objects
  --router <name>   Only "%s" or only "%s"
`

func runRoutes(args []string) error {
	fs := flag.NewFlagSet("routes", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	asJSON := fs.Bool("json", false, "emit JSON")
	only := fs.String("router", "", "limit to one listener")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, routesUsage, api.RouterPlatform, api.RouterApps)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch *only {
	case "", api.RouterPlatform, api.RouterApps:
	default:
		return fmt.Errorf("--router is %q or %q, not %q", api.RouterPlatform, api.RouterApps, *only)
	}

	routes, err := api.RouteInventory()
	if err != nil {
		return err
	}
	if *only != "" {
		kept := routes[:0]
		for _, r := range routes {
			if r.Router == *only {
				kept = append(kept, r)
			}
		}
		routes = kept
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(routes)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, r := range routes {
		fmt.Fprintf(w, "%s\t%s\t%s\n", r.Router, r.Method, r.Pattern)
	}
	return w.Flush()
}
