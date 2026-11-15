package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"toft/internal/appint"
	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/entitlements"
	"toft/internal/integrations"
	mailer "toft/internal/mail"
)

// Administering one app's integrations.
//
// Who may do what here follows one line, and the catalog did not change it:
// **attaching is an administrator's decision, configuring what is attached
// belongs to the app's people.** An owner who could enable their own app's
// email could mail the company from an address the company trusts; an owner who
// cannot choose their own channel is being asked to file a ticket for a
// preference.
//
// So: attach and detach are admin-only. The destination and the test button are
// `ActionEditContent` on the app. Reading the summary is anyone who can read
// the app (ADR §20) and rides on the policy block, not on these routes.
//
// There is one switch per kind and no per-action switch beneath it — an app
// that may post to Slack may upload a file, and one that may send mail may
// attach one. See appint's catalog and migration 44.

// ------------------------------------------------------- the app's own view

// appIntegrationRow is one kind, as this app sees it.
type appIntegrationRow struct {
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Summary string `json:"summary"`

	// Connected reports whether the platform offers this kind at all. Without
	// it a card cannot tell "your administrator has not attached this" from
	// "this install does not do that", which are different asks.
	Connected bool `json:"connected"`
	Attached  bool `json:"attached"`
	Enabled   bool `json:"enabled"`

	ConnectionName string `json:"connection_name,omitempty"`
	ScopeLabel     string `json:"scope_label,omitempty"`

	// Destination is where this app's sends land, and DestinationLabel is the
	// noun for it. DefaultDestination names where they go while the app's own
	// is unset — named rather than reported as an absence, because "posting to
	// #admin" is something an owner can act on and "no channel set" reads like
	// a broken integration.
	Destination        database.Destination `json:"destination"`
	DestinationLabel   string               `json:"destination_label,omitempty"`
	DefaultDestination database.Destination `json:"default_destination"`
	// Enumerates says whether there is a menu to choose from, or whether the
	// destination is fixed by the connection's scope.
	Enumerates bool `json:"enumerates"`

	Actions []appint.Action `json:"actions"`
	// LeavesResidue is what the attach dialog's copy turns on: this kind puts
	// objects in somebody else's system, and detaching will not remove them.
	LeavesResidue bool `json:"leaves_residue"`
	// IssuesTokens is the attached connection's token switch: this app may get
	// the connection's own access token, which reaches its whole grant.
	IssuesTokens bool `json:"issues_tokens"`
}

type appIntegrationsResponse struct {
	Integrations []appIntegrationRow `json:"integrations"`
	// Capabilities is the same list /app/self gives the app itself, so the tab
	// and the running code cannot disagree about what is switched on.
	Capabilities []string `json:"capabilities"`
	// ReplyToFallback is where this app's mail replies go while its people have
	// not chosen: the app owner's own address, or the install's reply-to for a
	// group-owned app. Here so the field's placeholder can say what leaving it
	// blank actually means, and ReplyToFallbackKind — "owner" or "install" —
	// says which of the two it is.
	ReplyToFallback     string `json:"reply_to_fallback"`
	ReplyToFallbackKind string `json:"reply_to_fallback_kind"`
}

// handleGetAppIntegrations is the Integrations tab's data.
//
// ActionEditContent rather than a read: this is the page that leads to the
// destination menu, and there is no reason for everyone who can *see* an app to
// learn which workspace channel it posts to. The read-only summary for them
// rides on GET /apps/{id}'s policy block.
func (s *Server) handleGetAppIntegrations(w http.ResponseWriter, r *http.Request) {
	app, ok := s.authorizeAppIntegration(w, r)
	if !ok {
		return
	}
	resp, err := s.appIntegrations(app)
	if err != nil {
		log.Printf("integrations: reading %s: %v", app.Slug, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) appIntegrations(app *database.App) (appIntegrationsResponse, error) {
	resp := appIntegrationsResponse{Integrations: []appIntegrationRow{}}

	for i := range appint.Known {
		kind := &appint.Known[i]
		attachment, err := s.db.ResolveAttachment(app.ID, kind.Name)
		if err != nil {
			return resp, err
		}
		row := appIntegrationRow{
			Kind:             kind.Name,
			Title:            kind.Title,
			Summary:          kind.Summary,
			Connected:        attachment.Connected,
			Attached:         attachment.Attached,
			Enabled:          attachment.Enabled,
			Destination:      attachment.Destination,
			DestinationLabel: kind.DestinationLabel,
			Enumerates:       kind.Enumerates,
			Actions:          kind.Actions,
			LeavesResidue:    kind.LeavesResidue,
			IssuesTokens:     attachment.Attached && attachment.IssueTokens,
		}
		if attachment.ConnectionID != "" {
			if conn, err := s.db.GetConnection(attachment.ConnectionID); err == nil && conn != nil {
				row.ConnectionName = conn.Name
				row.ScopeLabel = conn.ScopeLabel
				row.DefaultDestination = conn.DefaultDestination
			}
		}
		// A kind nobody has connected is absent from the tab entirely — absent,
		// not off. An app's people are not owed a list of what their
		// administrator has decided not to buy.
		if !row.Connected && !row.Attached {
			continue
		}
		// A static app is refused the attach (handleAttachAppIntegration), so
		// offering it one was a control that could only ever answer 409
		// (docs/bugs 2026-10-30 O1). One attached before that rule stays
		// listed, so it can still be switched off.
		if app.Framework == frameworkStatic && !row.Attached {
			continue
		}
		resp.Integrations = append(resp.Integrations, row)
	}

	caps, err := appint.Capabilities(s.db, app.ID)
	if err != nil {
		return resp, err
	}
	resp.Capabilities = caps
	resp.ReplyToFallback, resp.ReplyToFallbackKind = s.appReplyTo(app.ID)
	return resp, nil
}

func (s *Server) writeAppIntegrations(w http.ResponseWriter, app *database.App) {
	resp, err := s.appIntegrations(app)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ----------------------------------------------------------------- attaching

type attachRequest struct {
	// ConnectionID is which connection of this kind. Required when the install
	// has more than one and defaulted when it has exactly one — a form that
	// makes somebody choose between one option is a form that wastes a click.
	ConnectionID string `json:"connection_id"`
	// Enabled defaults to true on an attach: an administrator who attaches an
	// integration means it to be used.
	Enabled *bool `json:"enabled,omitempty"`
}

// handleAttachAppIntegration gives an app one kind, or flips the switch on one
// it already has.
//
// This is the gate the whole design turns on. An app with no row here has no
// integration of that kind at all — no field on its tab, no destination to
// resolve, no possibility of a send — because the destination menu is the
// organization's, and being able to enumerate it should follow a decision
// somebody made rather than being ambient for every publisher on the install.
func (s *Server) handleAttachAppIntegration(w http.ResponseWriter, r *http.Request) {
	p := s.adminPrincipal(w, r)
	if p == nil {
		return
	}
	if s.refusedUnlicensed(w, r, entitlements.FeatureIntegrations) {
		return
	}
	app, kind, ok := s.appKindParams(w, r)
	if !ok {
		return
	}

	var req attachRequest
	// An empty body is a plain attach, so a decode failure on one is not worth
	// refusing.
	_ = json.NewDecoder(r.Body).Decode(&req)

	existing, err := s.db.GetAppIntegration(app.ID, kind.Name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	row := &database.AppIntegration{AppID: app.ID, Kind: kind.Name, Enabled: true}
	if existing != nil {
		row = existing
	}
	if req.Enabled != nil {
		row.Enabled = *req.Enabled
	}
	// A static app has no process and no job to send anything from, which is
	// why storage refuses it (validateAttach): accepted, the attachment
	// read `attached: true` with `email.send` among its capabilities, and
	// nothing on the app could ever use it (docs/bugs 2026-10-28 D13). Only
	// the attach that would leave one switched on is refused, so one made
	// before this rule can still be switched off.
	if row.Enabled && app.Framework == frameworkStatic {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "this is a static app: a folder of files the platform serves, with no process " +
				"or job to send from. Deploy it as an app in a language Toft supports to attach " + kind.Title + ".",
			"code": "static_app",
			"hint": "attach it to an app with a process or jobs; to send from this one, redeploy it in a language Toft runs"})
		return
	}

	if connID := strings.TrimSpace(req.ConnectionID); connID != "" {
		conn, err := s.db.GetConnection(connID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if conn == nil || conn.Kind != kind.Name {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "that is not a " + kind.Title + " connection on this install",
				"hint":  "send connection_id with the id of a " + kind.Title + " connection from GET /api/v1/admin/integrations/connections"})
			return
		}
		// **One attachment per (app, kind), and a second is refused rather than
		// swapped.** That is what /app/self's capability list can express:
		// `webhook.post` is a capability and "webhook.post via the connection
		// called ops-relay" is not, and the moment an app has to name *which*
		// webhook it has chosen a destination — the one thing ADR §25 forbids
		// it.
		//
		// Refused rather than silently re-pointed, because re-pointing is the
		// same disclosure the welded scope exists to prevent: the app carries
		// on calling the same route and its messages start arriving somewhere
		// else. Detaching first is the visible version of that decision.
		if existing != nil && existing.ConnectionID != conn.ID {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "this app is already on the " + kind.Title + " connection called " +
					strconv.Quote(existing.ConnectionName) +
					" — detach it first, so nothing it sends moves without anybody noticing",
				"code": "already_attached",
				"hint": "DELETE /api/v1/apps/" + app.ID + "/integrations/" + kind.Name + " detaches it; then attach again"})
			return
		}
		row.ConnectionID = conn.ID
	} else if row.ConnectionID == "" {
		conns, err := s.db.ListConnectionsOfKind(kind.Name)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		switch len(conns) {
		case 0:
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": kind.Title + " is not connected on this install — connect it in " +
					"Settings › Integrations first",
				"code": kind.Name + "_off",
				"hint": "POST /api/v1/admin/integrations/connections connects " + kind.Title + "; then attach again"})
			return
		case 1:
			row.ConnectionID = conns[0].ID
		default:
			names := make([]string, 0, len(conns))
			for _, c := range conns {
				names = append(names, c.Name)
			}
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "this install has more than one " + kind.Title +
					" connection — say which: " + strings.Join(names, ", "),
				"code": "connection_required",
				"hint": "send connection_id with the id of the one you mean — GET /api/v1/admin/integrations/connections lists them"})
			return
		}
	}

	if err := s.db.AttachAppIntegration(row, p.ID, requestMeta(r.Context())); err != nil {
		log.Printf("integrations: attaching %s to %s: %v", kind.Name, app.Slug, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	s.writeAppIntegrations(w, app)
}

// handleDetachAppIntegration removes the attachment and forgets the destination
// its people chose.
//
// The reversible half is an attach with enabled=false, which stops the sends and
// keeps the choice. One is a toggle; this destroys something a person entered —
// and it stops sends already sitting in the queue, because off means now.
//
// **It reaches nothing already written.** Where the kind leaves residue, what
// this app has put in somebody else's system stays there, and the connection's
// own page is where an administrator can see what that is.
func (s *Server) handleDetachAppIntegration(w http.ResponseWriter, r *http.Request) {
	p := s.adminPrincipal(w, r)
	if p == nil {
		return
	}
	app, kind, ok := s.appKindParams(w, r)
	if !ok {
		return
	}
	if err := s.db.DetachAppIntegration(app.ID, kind.Name, p.ID, requestMeta(r.Context())); err != nil {
		log.Printf("integrations: detaching %s from %s: %v", kind.Name, app.Slug, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"attached": false, "kind": kind.Name})
}

// ------------------------------------------------- the app team's own choice

type setDestinationRequest struct {
	// ID empty means "use the connection's default", which is a state an app's
	// people have to be able to get back to.
	ID string `json:"id"`
	// Label is accepted only for a kind that enumerates nothing and validates
	// its destination itself — mail's reply-to is the one that exists today.
	Label string `json:"label"`
}

// handleSetAppIntegrationDestination records the destination an app's own
// people chose, inside the administrator's scope.
//
// ActionEditContent, not admin: which channel this app posts to, or which
// address answers its mail, is the app team's business, and routing it through
// an administrator would make a preference into a ticket. What they cannot do
// is *grant* the capability.
func (s *Server) handleSetAppIntegrationDestination(w http.ResponseWriter, r *http.Request) {
	app, kind, ok := s.authorizeAppKind(w, r)
	if !ok {
		return
	}
	// Where the app posts is what it runs with when the lock lifts, the same
	// class as its environment (security review 2026-11-15, LSA-2).
	if s.refuseIfLocked(w, app) {
		return
	}
	row, err := s.db.GetAppIntegration(app.ID, kind.Name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if row == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "this app does not have a " + kind.Title +
				" integration — an administrator adds one",
			"hint": "an administrator can attach it: PUT /api/v1/apps/" + app.ID + "/integrations/" + kind.Name})
		return
	}
	if kind.DestinationLabel == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": kind.Title + " has one destination, fixed when the connection was made",
			"code":  "welded",
			"hint":  "leave the destination as it is — to send somewhere else, an administrator makes another connection"})
		return
	}

	var req setDestinationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body", "hint": badBodyHint})
		return
	}
	req.ID = strings.TrimSpace(req.ID)

	dest := database.Destination{}
	switch {
	case req.ID == "":
		// Back to the connection's default.
	case kind.Enumerates:
		// Checked against the connection's own list before it is stored. Not
		// because a bad id is dangerous — the far side would simply refuse the
		// send — but because the refusal would happen hours later, in a log
		// nobody reads, at the exact moment the message mattered. The check also
		// pins the label to what the far side calls it rather than what somebody
		// typed.
		label, err := s.destinationLabel(r, row.ConnectionID, kind, req.ID)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		if label == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "that is not a " + kind.DestinationLabel + " Toft can reach — " +
					"the connection's own list is the boundary",
				"hint": "GET /api/v1/apps/" + app.ID + "/integrations/" + kind.Name + "/destinations lists the ones you can choose"})
			return
		}
		dest = database.Destination{ID: req.ID, Label: label}
	case kind.Name == appint.IntegrationEmail:
		// Mail's destination is a reply-to address, which is validated here
		// rather than at send time: an address that cannot be parsed should be
		// refused while somebody is looking at the field, not six weeks later
		// inside a queue worker.
		addr, err := mailer.ValidateRecipient(req.ID)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": `send one address, as {"id": "reports@example.com"}, or an empty id for the connection's default`})
			return
		}
		dest = database.Destination{ID: addr, Label: addr}
	default:
		dest = database.Destination{ID: req.ID, Label: strings.TrimSpace(req.Label)}
	}

	p := auth.GetPrincipal(r.Context())
	actor := ""
	if p != nil {
		actor = p.ID
	}
	if err := s.db.SetAppIntegrationDestination(app.ID, kind.Name, dest, actor,
		requestMeta(r.Context())); err != nil {
		log.Printf("integrations: setting %s's %s destination: %v", app.Slug, kind.Name, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	s.writeAppIntegrations(w, app)
}

// handleAppIntegrationDestinations is the second authorized front door onto a
// connection's destination list — scoped to people who may edit an app that has
// the kind attached.
//
// The attachment is what makes the organization's list visible to an app's
// people. Without it there is nothing to show and nothing to choose, and saying
// so beats an empty menu.
func (s *Server) handleAppIntegrationDestinations(w http.ResponseWriter, r *http.Request) {
	app, kind, ok := s.authorizeAppKind(w, r)
	if !ok {
		return
	}
	row, err := s.db.GetAppIntegration(app.ID, kind.Name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if row == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "this app does not have a " + kind.Title +
				" integration — an administrator adds one",
			"hint": "an administrator can attach it: PUT /api/v1/apps/" + app.ID + "/integrations/" + kind.Name})
		return
	}
	conn, err := s.db.GetConnection(row.ConnectionID)
	if err != nil || conn == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	s.writeDestinations(w, r, conn, kind)
}

// destinationLabel is what the far side calls one destination, or "" when it is
// not one this connection can reach.
func (s *Server) destinationLabel(r *http.Request, connID string,
	kind *appint.Kind, id string) (string, error) {

	lister, ok := s.transports[kind.Name].(integrations.Enumerator)
	if !ok {
		return "", errors.New(kind.Title + " is not available on this install")
	}
	target, err := s.connectionTarget(connID, database.Destination{})
	if err != nil {
		return "", err
	}
	list, err := lister.Destinations(r.Context(), target)
	if err != nil {
		return "", err
	}
	for _, d := range list {
		if d.ID == id {
			return d.Label, nil
		}
	}
	return "", nil
}

// --------------------------------------------------------------- test sends

// handleTestAppIntegration sends a real message through this app's own identity.
//
// Deliberately not a special path. What somebody needs to know is that *this
// app's* attachment, *this app's* destination and *this app's* sender identity
// work end to end — a test that bypassed any of them would prove the wrong
// thing. So it runs the same gates, writes the same ledger row, and shows up in
// the Integration log like every other send.
func (s *Server) handleTestAppIntegration(w http.ResponseWriter, r *http.Request) {
	app, ok := s.authorizeAppIntegration(w, r)
	if !ok {
		return
	}
	// A test send is a real message; nothing is sent for a locked app, and
	// its status cannot say so — locking does not stop a batch app (security
	// review 2026-11-15, LSA-2).
	if s.refuseIfLocked(w, app) {
		return
	}
	var req struct {
		Kind string `json:"kind"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body", "hint": badBodyHint})
		return
	}
	kind := appint.Lookup(strings.TrimSpace(req.Kind))
	if kind == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": strconv.Quote(req.Kind) + " is not an integration this platform offers",
			"code":  "unknown_kind"})
		return
	}

	// Who pressed the button, for the message body and for the default
	// recipient. A Principal carries no name or address — it is an
	// authorization subject, not a profile — so the user row is read here.
	p := auth.GetPrincipal(r.Context())
	who, actorEmail := "an administrator", ""
	if p != nil {
		if u, err := s.db.GetUser(p.ID); err == nil && u != nil {
			if u.Name != "" {
				who = u.Name
			}
			actorEmail = u.Email
		}
	}

	action := kind.Actions[0].Name
	payload := appint.Payload{
		Text: "Test message from " + app.Name + ", sent by " + who +
			" from Toft. If you can read this, this app's " + kind.Title +
			" integration works.",
		Severity: "info",
		DedupKey: "toft-test",
	}
	// A file-only kind can do exactly one thing, so the test does that thing:
	// a one-line text file lands in the default destination. A text payload
	// was refused by the transport — "this integration takes a file" — after
	// a 202 and a toast saying the test had been sent.
	if kind.Actions[0].Carries == appint.CarriesFile {
		payload.Files = []appint.File{testFile(payload.Text)}
	}
	record := sendRecord{}

	if kind.Name == appint.IntegrationEmail {
		to := strings.TrimSpace(req.To)
		if to == "" {
			// Defaults to whoever pressed the button, which is almost always
			// what they meant and never a surprise recipient.
			to = actorEmail
		}
		addr, err := mailer.ValidateRecipient(to)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "a test needs an address to send to — " + err.Error(),
				"hint":  `send one address, as "to": "you@example.com"`})
			return
		}
		payload.Subject = "Test message from " + app.Name
		payload.To = []string{addr}
		record.Recipients = []string{strings.ToLower(addr)}
		record.Subject = payload.Subject
	}

	digest, size := appint.Digest(payload.Subject, payload.Text)
	record.BodySHA256, record.BodyBytes, record.Body = digest, size, payload.Text

	capability := kind.Capability(action)
	decision, err := appint.Check(s.db, app.ID, app.Status, capability)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if !decision.Allowed {
		writeJSON(w, decision.Status, decisionBody(decision))
		return
	}

	send, err := s.queueSend(sendRequest{
		AppID:      app.ID,
		AppStatus:  app.Status,
		AppName:    app.Name,
		AppSlug:    app.Slug,
		Capability: capability,
		Payload:    payload,
		Record:     record,
	})
	if errors.Is(err, appint.ErrQueueFull) {
		// The app route's answer to the same refusal: wait and send again.
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{
			"error": err.Error() + " — try again in a few seconds",
			"code":  "queue_full",
			"hint":  "wait a few seconds, honoring Retry-After, and send again"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"send_id": send.ID, "status": send.Status})
}

// ------------------------------------------------------------------ the log

// handleListAppSends is one app's recent sends, for its Integrations tab.
//
// The app's own people see their own app's recipients, which they chose; the
// cross-app view is an administrator's and lives on the Integration log.
func (s *Server) handleListAppSends(w http.ResponseWriter, r *http.Request) {
	app, ok := s.authorizeAppIntegration(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	sends, err := s.db.ListAppSends(app.ID, r.URL.Query().Get("action"), limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"sends": sends})
}

// handleAdminIntegrationSends is the install-wide ledger.
//
// This page is the control, not a convenience. Recipients are unrestricted by
// decision, which makes the record the only thing between an app and any
// address on the internet — and the filter that matters is by recipient,
// because "which app emailed this person" is how an incident actually opens.
//
// It keeps working while every connection is switched off, for the reason
// /admin/api-keys does: that is exactly when a leaked one has to be found.
func (s *Server) handleAdminIntegrationSends(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	q := r.URL.Query()
	filter := database.SendFilter{
		AppID:     q.Get("app_id"),
		Kind:      q.Get("kind"),
		Action:    q.Get("action"),
		Status:    q.Get("status"),
		Recipient: q.Get("recipient"),
	}
	filter.Limit, _ = strconv.Atoi(q.Get("limit"))
	if days, err := strconv.Atoi(q.Get("days")); err == nil && days > 0 {
		since := time.Now().UTC().AddDate(0, 0, -days)
		filter.Since = &since
	}

	sends, err := s.db.ListSends(filter)
	if err != nil {
		log.Printf("integration log: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	settings, _ := s.db.GetIntegrationSettings()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"sends":          sends,
		"kinds":          appint.KindNames(),
		"kind_titles":    appint.KindTitles(),
		"retention_days": settings.RetentionDays,
	})
}

// ------------------------------------------------------- the ledger's settings

// handleGetIntegrationSettings reports how long the install keeps the record.
//
// 404 to non-admins, matching /system/settings and the email document: how long
// this install keeps evidence of what its apps have sent is infrastructure
// detail. What an app's own people are owed is the decision as it applies to
// *their* app, which is the policy block.
func (s *Server) handleGetIntegrationSettings(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	settings, err := s.db.GetIntegrationSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) handleUpdateIntegrationSettings(w http.ResponseWriter, r *http.Request) {
	p := s.adminPrincipal(w, r)
	if p == nil {
		return
	}
	current, err := s.db.GetIntegrationSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err := json.NewDecoder(r.Body).Decode(&current); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body", "hint": badBodyHint})
		return
	}
	if current.RetentionDays < database.MinSendRetentionDays {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "the send log has to be kept for at least " +
				strconv.Itoa(database.MinSendRetentionDays) + " days — it is the only record " +
				"of what apps have sent, and below that it stops being able to answer an incident",
			"hint": "send a retention_days of " + strconv.Itoa(database.MinSendRetentionDays) + " or more — GET /api/v1/admin/integration-settings shows the stored value"})
		return
	}
	if err := s.db.SetIntegrationSettings(&current, p.ID, requestMeta(r.Context())); err != nil {
		log.Printf("integration settings: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, current)
}

// ----------------------------------------------------------------- helpers

// authorizeAppIntegration resolves the app and authorizes the caller to change
// its settings, answering 404 to somebody who may not read it at all.
//
// One helper so the app-scoped integration handlers cannot drift into different
// answers about who may use them.
func (s *Server) authorizeAppIntegration(w http.ResponseWriter, r *http.Request) (*database.App, bool) {
	appID := chi.URLParam(r, "appID")
	app, ok := s.appOr404(w, appID)
	if !ok {
		return nil, false
	}
	p := auth.GetPrincipal(r.Context())
	obj := objectFromApp(app)
	readable := auth.CanRead(p, obj, s.db)
	d := auth.Authorize(p, auth.ActionEditContent, obj, s.db)
	if !d.Allow {
		writeAuthzError(w, r, d, readable)
		return nil, false
	}
	return app, true
}

// authorizeAppKind is authorizeAppIntegration plus the {kind} parameter.
func (s *Server) authorizeAppKind(w http.ResponseWriter, r *http.Request) (
	*database.App, *appint.Kind, bool) {

	app, ok := s.authorizeAppIntegration(w, r)
	if !ok {
		return nil, nil, false
	}
	kind := appint.Lookup(chi.URLParam(r, "kind"))
	if kind == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": strconv.Quote(chi.URLParam(r, "kind")) +
				" is not an integration this platform offers",
			"code": "unknown_kind"})
		return nil, nil, false
	}
	return app, kind, true
}

// appKindParams is the admin half: the app and the kind, with no per-app
// authorization because the caller has already been checked as an
// administrator.
func (s *Server) appKindParams(w http.ResponseWriter, r *http.Request) (
	*database.App, *appint.Kind, bool) {

	app, ok := s.appOr404(w, chi.URLParam(r, "appID"))
	if !ok {
		return nil, nil, false
	}
	kind := appint.Lookup(chi.URLParam(r, "kind"))
	if kind == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": strconv.Quote(chi.URLParam(r, "kind")) +
				" is not an integration this platform offers",
			"code": "unknown_kind"})
		return nil, nil, false
	}
	return app, kind, true
}

// testFileName is what a file-kind test uploads, so an administrator can find
// it in the destination and delete it.
const testFileName = "toft-test.txt"

// testFile is the one-line text file a file-kind test sends.
func testFile(line string) appint.File {
	return appint.File{Name: testFileName, Type: "text/plain", Content: []byte(line + "\n")}
}
