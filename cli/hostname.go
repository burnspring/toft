package main

import (
	"encoding/json"
	"net/http"
)

// toft hostname: an app on a hostname of its own. A rendering of the fields
// PATCH /apps/{id} already takes — the install decides who may set them (an
// administrator) and which names it refuses, and says so when it does.

const hostnameUsage = `toft hostname — an app on a hostname of its own

Usage:
  toft hostname                      Print the name, whether it is on, and the address
  toft hostname <name>               Record a name (it is not served until switched on)
  toft hostname --on | --off         Serve the recorded name, or stop serving it
  toft hostname <name> --on          Both at once
  toft hostname --clear              Forget the name (and stop serving it)

Flags:
  --app <slug>        Which app
  --json              Machine-readable output on stdout

Administrators only. Switching on or off — or a new name while on — rebuilds
the code already running at its new address; the old address keeps serving
until the rebuild finishes, then redirects. The name's DNS must point at this
install's apps address first, and TLS ends upstream.
`

type hostnameState struct {
	Hostname        string `json:"hostname"`
	HostnameEnabled bool   `json:"hostname_enabled"`
	PublicURL       string `json:"public_url"`
}

func cmdHostname(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("hostname", hostnameUsage)
	g.register(fs)
	on := fs.Bool("on", false, "serve the recorded name")
	off := fs.Bool("off", false, "stop serving it")
	clear := fs.Bool("clear", false, "forget the name")
	name, rest := takeOperand(args)
	if err := parseAfter(fs, rest, name); err != nil {
		return skipHelp(err)
	}
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	if *on && *off {
		return usageCodef("conflicting_flags", "--on and --off together say nothing")
	}
	if *clear && (name != "" || *on) {
		return usageCodef("conflicting_flags", "--clear forgets the name; it cannot also set or serve one")
	}
	s, app, err := jobSession(e, &g)
	if err != nil {
		return err
	}

	body := map[string]any{}
	switch {
	case *clear:
		body["hostname"] = ""
	case name != "":
		body["hostname"] = name
	}
	if *on {
		body["hostname_enabled"] = true
	} else if *off {
		body["hostname_enabled"] = false
	}
	read := func() (hostnameState, error) {
		var got struct {
			App hostnameState `json:"app"`
		}
		err := s.client.Do(http.MethodGet, "/api/v1/apps/"+app.ID+"?view=brief", nil, &got)
		return got.App, err
	}
	// changed is what the state became, not whether something was asked: an
	// --off on a name already off changes nothing and says so (BB-026).
	var before hostnameState
	if len(body) > 0 {
		if before, err = read(); err != nil {
			return err
		}
		if err := s.client.Do(http.MethodPatch, "/api/v1/apps/"+app.ID, body, nil); err != nil {
			return err
		}
	}
	h, err := read()
	if err != nil {
		return err
	}
	changed := len(body) > 0 && (before.Hostname != h.Hostname || before.HostnameEnabled != h.HostnameEnabled)
	if g.json {
		return json.NewEncoder(e.out.Stdout).Encode(map[string]any{
			"app": app.Slug, "hostname": h.Hostname, "hostname_enabled": h.HostnameEnabled,
			"public_url": h.PublicURL, "changed": changed})
	}
	switch {
	case h.Hostname == "":
		e.out.Answer("%s  no hostname — served at %s", e.out.Bold(app.Slug), h.PublicURL)
	case h.HostnameEnabled:
		e.out.Answer("%s  %s (on) — served at %s", e.out.Bold(app.Slug), h.Hostname, h.PublicURL)
	default:
		e.out.Answer("%s  %s (off) — served at %s", e.out.Bold(app.Slug), h.Hostname, h.PublicURL)
	}
	if changed && h.Hostname != "" && h.PublicURL != "" {
		e.out.Hint("A move rebuilds the running code; until it finishes the app answers at its old address.")
	}
	return nil
}
