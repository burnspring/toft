package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"toft/cli/internal/ui"
)

const storageUsage = `toft storage — storage an app can be given

Usage:
  toft storage ls              The storage you can attach, and your ceiling on each
  toft storage ls --app <slug> What that app has attached, and where each came from

Flags:
  --app <slug>        Which app
  --json              Machine-readable output on stdout

An administrator creates every storage resource and folder. An app gets one by
declaring it in toft.toml:

  [[storage]]
  name   = "sales-data"
  access = "read"            # read (default), write or full
  folder = "2026"            # optional: one folder of the resource
  path   = "/data/sales"     # optional: default /storage/<name>[/<folder>]

The app's owner has to be able to attach each entry — a group-owned app through
the group's own delegation — and whoever deploys has to be able to as well when
the deploy adds one or raises its level. ` + "`toft check`" + ` shows the plan before
a deploy does.

This is about attaching storage to apps. To read and write its files from
outside — an object store, or a volume on an install without a cluster — use
` + "`toft store`" + `: toft store put <name> <file>, toft store cp -r ./dir toft://<name>/.
`

func cmdStorage(e *env, args []string) error {
	if len(args) == 0 {
		ui.Help(os.Stderr, storageUsage)
		return usageCodef("missing_argument", "toft storage needs a subcommand: ls")
	}
	switch args[0] {
	case "-h", "--help", "help":
		ui.Help(os.Stdout, storageUsage)
		return nil
	case "ls":
		return cmdStorageList(e, args[1:])
	}
	return unknownSubcommand("storage", args, "ls")
}

// attachableStorage is one row of GET /me/storage, whose answer is
// {"resources": [...], "shared_mode": bool}.
type attachableStorage struct {
	Slug        string `json:"slug"`
	Kind        string `json:"kind"`
	Description string `json:"description"`
	MaxAccess   string `json:"max_access"`
	Partitioned bool   `json:"partitioned"`
}

// appStorage is GET /apps/{id}/storage.
type appStorage struct {
	Mappings []struct {
		ResourceSlug string `json:"resource_slug"`
		ResourceKind string `json:"resource_kind"`
		Folder       string `json:"folder"`
		Access       string `json:"access"`
		Path         string `json:"path"`
		Source       string `json:"source"`
	} `json:"mappings"`
	Pending bool   `json:"pending"`
	Note    string `json:"note"`
}

func cmdStorageList(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("storage ls", storageUsage)
	g.register(fs)
	if err := parse(fs, args); err != nil {
		return skipHelp(err)
	}
	s, err := e.setup(&g)
	if err != nil {
		return err
	}
	if g.app != "" {
		app, err := s.resolveApp(g.app)
		if err != nil {
			return err
		}
		return listAppStorage(e, s, &g, app)
	}

	var raw json.RawMessage
	if err := s.client.Do(http.MethodGet, "/api/v1/me/storage", nil, &raw); err != nil {
		return err
	}
	if g.json {
		fmt.Fprintln(e.out.Stdout, strings.TrimSpace(string(raw)))
		return nil
	}
	var got struct {
		Resources []attachableStorage `json:"resources"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		return fmt.Errorf("this install sent a storage list this version of toft does not understand — upgrade toft (%v)", err)
	}
	rows := got.Resources
	if len(rows) == 0 {
		e.out.Say("You can attach no storage on this install. An administrator lets you attach a resource, " +
			"or creates one with you as a default.")
		return nil
	}
	t := e.out.NewTable("NAME", "KIND", "UP TO", "NOTE").Flex(3)
	for _, r := range rows {
		note := r.Description
		if r.Partitioned {
			note = strings.TrimSpace("by folder — name one in toft.toml. " + note)
		}
		t.Row(r.Slug, r.Kind, r.MaxAccess, note)
	}
	t.Print()
	return nil
}

func listAppStorage(e *env, s *session, g *globalFlags, app *appSummary) error {
	var raw json.RawMessage
	if err := s.client.Do(http.MethodGet, "/api/v1/apps/"+app.ID+"/storage", nil, &raw); err != nil {
		return err
	}
	if g.json {
		fmt.Fprintln(e.out.Stdout, strings.TrimSpace(string(raw)))
		return nil
	}
	var got appStorage
	if err := json.Unmarshal(raw, &got); err != nil {
		return fmt.Errorf("this install sent app storage this version of toft does not understand — upgrade toft (%v)", err)
	}
	if len(got.Mappings) == 0 {
		e.out.Say("%s has no storage attached.", app.Slug)
		return nil
	}
	t := e.out.NewTable("NAME", "KIND", "ACCESS", "PATH", "FROM").Flex(3)
	for _, m := range got.Mappings {
		name := m.ResourceSlug
		if m.Folder != "" {
			name += "/" + m.Folder
		}
		from := "console"
		if m.Source == "manifest" {
			from = "toft.toml"
		}
		t.Row(name, m.ResourceKind, m.Access, m.Path, from)
	}
	t.Print()
	if got.Pending && got.Note != "" {
		e.out.Dim("%s", got.Note)
	}
	return nil
}
