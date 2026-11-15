package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"toft/internal/api"
)

// `toft openapi` — the route inventory as an OpenAPI document.
//
// Beside `toft routes` and for the same reason: building a chi router is
// pure, so this is a question the binary can answer about itself with no
// database, no configuration and no server. The difference between the two is
// only the shape of the answer — `routes` is for a person and for the crawl in
// this repository, and this is for a scanner somebody else runs.
//
// It exists as a command *as well as* an admin route because a pipeline that
// has the binary should not have to hold a credential to ask the binary what it
// serves, and an AppSec team that has a credential should not need a shell.
// Same generator, so the two cannot disagree.

const openAPIUsage = `Usage: toft openapi [flags]

Print this binary's platform API as an OpenAPI 3.1 document: every path, every
method, every path parameter, and no schemas. Reads no configuration and opens
no database.

It is a target list for scanners and inventory tools, not an API contract.

Flags:
  --server <url>    The install's own address, written into servers[]. Omitted
                    by default: a generated file cannot know where it will be
                    served from, and a wrong server URL is worse than none.
`

func runOpenAPI(args []string) error {
	fs := flag.NewFlagSet("openapi", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	server := fs.String("server", "", "the install's own address")
	fs.Usage = func() { fmt.Fprint(os.Stderr, openAPIUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}

	doc, err := api.OpenAPIDocument(*server)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}
