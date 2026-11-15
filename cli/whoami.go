package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
)

// `toft whoami` — who this credential is, what it may do, and what this install
// will do with a deploy.
//
// **An unattended caller should learn its limits by asking.** Every fact here
// was previously learned from a refusal: a read-scoped key at its first deploy,
// a viewer at `toft create`, an install with uploads off at the upload, a pin
// at the build. One call answers all of them before any of them is tried.
//
// It renders `GET /me/capabilities`, and under --json passes the install's
// document through beside the identity — the `toft deps` rule, so a field
// this binary is too old to name still reaches the caller.

const whoamiUsage = `toft whoami — who you are on this install, and what it will let you do

Prints the account and the credential (its kind, scope and expiry), whether
you can create apps, and what the install allows: uploaded bundles, public
apps, background jobs, and the interpreter versions a new app is judged
against.

Flags:
  --install <url>     Which install
  --json              The install's answer, with the identity beside it
  --session           As the stored password session, not the key
`

type capabilities struct {
	SystemRole     string `json:"system_role"`
	CanCreate      bool   `json:"can_create_objects"`
	CanRequestRole bool   `json:"can_request_publisher_role"`
	NewAppMode     string `json:"new_app_execution_mode"`
	Credential     struct {
		Kind      string     `json:"kind"`
		Name      string     `json:"name"`
		Scope     string     `json:"scope"`
		ExpiresAt *time.Time `json:"expires_at"`
	} `json:"credential"`
	Install *struct {
		Version string `json:"version"`
		Uploads bool   `json:"uploads_allowed"`
		Public  bool   `json:"public_apps_allowed"`
		Jobs    bool   `json:"jobs_available"`
		// Stores is whether `toft store` and the SDKs' signed-URL calls have a
		// data plane here. A pointer: an install older than the field says
		// nothing, and "no" would be a claim it did not make.
		Stores   *bool               `json:"store_uploads_available"`
		SDKs     []string            `json:"sdk_languages"`
		Runtimes map[string][]string `json:"runtimes"`
	} `json:"install"`
}

func cmdWhoami(e *env, args []string) error {
	var g globalFlags
	fs := newFlags("whoami", whoamiUsage)
	g.register(fs)
	if err := parse(fs, args); err != nil {
		return skipHelp(err)
	}
	s, err := e.setup(&g)
	if err != nil {
		return err
	}

	var raw json.RawMessage
	if err := s.client.Do(http.MethodGet, "/api/v1/me/capabilities", nil, &raw); err != nil {
		return err
	}
	var caps capabilities
	if err := json.Unmarshal(raw, &caps); err != nil {
		return err
	}
	// A deploy key belongs to nobody, and /auth/me is not on its list.
	var me struct {
		Login string `json:"login"`
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if !s.IsDeployKey() {
		if err := s.client.Do(http.MethodGet, "/api/v1/auth/me", nil, &me); err != nil {
			return err
		}
	}

	if g.json {
		doc := map[string]any{"install_url": s.InstallURL, "capabilities": raw}
		if me.Login != "" {
			doc["user"] = me
		}
		return json.NewEncoder(e.out.Stdout).Encode(doc)
	}

	who := orDefault(me.Email, me.Login)
	if s.IsDeployKey() {
		who = "deploy key " + caps.Credential.Name
	}
	role := caps.SystemRole
	if role == "" {
		role = "no system role"
	}
	e.out.Answer("%s  %s  %s", e.out.Bold(who), role, e.out.Dimmed("on "+s.InstallURL))

	cred := caps.Credential.Kind
	if caps.Credential.Name != "" {
		cred += " " + caps.Credential.Name
	}
	if caps.Credential.Scope != "" {
		cred += " · " + caps.Credential.Scope + " scope"
	}
	switch {
	case caps.Credential.ExpiresAt != nil:
		cred += " · expires " + caps.Credential.ExpiresAt.Format("2006-01-02")
	case caps.Credential.Kind != "session" && caps.Credential.Kind != "":
		cred += " · does not expire"
	}
	e.out.Field(14, "credential", cred)

	create := e.out.Badge(yesNo(caps.CanCreate))
	if !caps.CanCreate && caps.CanRequestRole {
		create += e.out.Dimmed(" — POST /api/v1/me/promotion-requests asks an administrator")
	}
	if caps.CanCreate && s.IsDeployKey() {
		// It belongs to nobody, so what it creates is a group's.
		create += e.out.Dimmed(" — owned by a group: toft create --owner-group <name|id>")
	}
	e.out.Field(14, "create apps", create)
	if caps.Install == nil {
		e.out.Dim("This install is older than the rest of this answer.")
		return nil
	}
	in := caps.Install
	e.out.Field(14, "uploads", e.out.Badge(allowed(in.Uploads)))
	e.out.Field(14, "public apps", e.out.Badge(allowed(in.Public)))
	e.out.Field(14, "jobs", e.out.Badge(allowed(in.Jobs)))
	if in.Stores != nil {
		e.out.Field(14, "store uploads", e.out.Badge(allowed(*in.Stores)))
	}
	if caps.NewAppMode != "" {
		e.out.Field(14, "new apps run", caps.NewAppMode)
	}
	langs := make([]string, 0, len(in.Runtimes))
	for l := range in.Runtimes {
		langs = append(langs, l)
	}
	sort.Strings(langs)
	for _, l := range langs {
		v := strings.Join(in.Runtimes[l], ", ")
		if v == "" {
			v = e.out.Dimmed("none")
		}
		e.out.Field(14, l, v)
	}
	e.out.Dim("Install %s.", in.Version)
	return nil
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func allowed(b bool) string {
	if b {
		return "allowed"
	}
	return "not allowed"
}
