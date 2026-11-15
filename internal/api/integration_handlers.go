package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"toft/internal/appint"
	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/entitlements"
	"toft/internal/events"
	"toft/internal/integrations"
	"toft/internal/notify"
	slackpkg "toft/internal/slack"
)

// Administering integration connections.
//
// A connection is a credential, so every route here is an administrator's and
// every one of them is on the *denied* list in keyroutes.go: the standing rule
// is that a key never manages a credential, and eight kinds does not soften it.
// Non-admins get 404, matching /system/settings and the email page — which
// workspace, which site and which relay this install reaches is infrastructure
// detail.
//
// The credential goes in and never comes out, the same contract as an auth
// provider's client secret and the SMTP password, and for the same reason: an
// admin API that can read back a credential is a credential store with an HTTP
// interface.

// Slacker is the Server's view of the Slack service — the *platform
// notification* half only. App-initiated posts go through the catalog's
// transport registry and never through here.
//
// An interface rather than the concrete type so tests can record what would
// have been posted without a workspace, and so a nil one — the default — makes
// every notification a no-op instead of a panic.
type Slacker interface {
	// Enabled reports whether any Slack connection exists.
	Enabled() bool
	// BaseURL is where links in a notification should point.
	BaseURL() string
	// PostAsync delivers in the background and never fails the caller. It
	// reports whether the message passed the gates, never whether Slack
	// accepted it.
	PostAsync(template, appID string, d notify.TemplateData) bool
	// PostTest posts to one connection's default channel synchronously and
	// returns Slack's own error.
	PostTest(ctx context.Context, connID string) error
	// Channels lists the conversations one connection's bot is a member of.
	Channels(ctx context.Context, connID string) ([]slackpkg.Channel, error)
}

// WithSlack supplies the Slack service. Absent — which is every test that does
// not ask for one — the platform still records everything it would have posted
// about; it just says nothing out of band.
func WithSlack(s Slacker) ServerOption {
	return func(srv *Server) { srv.slack = s }
}

// WithPosters supplies the non-Slack posters a platform notification is also
// posted through — in production the one catalog poster, the same one the
// events recorder holds. Nil entries are dropped, as events.NewRecorder does.
func WithPosters(p ...events.Poster) ServerOption {
	return func(srv *Server) {
		for _, one := range p {
			if one != nil {
				srv.posters = append(srv.posters, one)
			}
		}
	}
}

// postPlatform posts one platform notification to every channel that carries
// it: Slack when there is one, then every other poster. build is called once
// per channel with that channel's own resolved base URL, so each link points
// where that channel says the install lives. An empty appID is an
// install-level notice; each poster applies its own audience and attachment
// gates.
func (s *Server) postPlatform(template, appID string, build func(baseURL string) notify.TemplateData) {
	if s.slack != nil {
		s.slack.PostAsync(template, appID, build(s.slack.BaseURL()))
	}
	for _, p := range s.posters {
		p.PostAsync(template, appID, build(p.BaseURL()))
	}
}

// ------------------------------------------------------------------- DTOs

// connectionDTO is what a connection looks like over the wire. Built by hand
// rather than by marshalling the row, so that adding a field to the struct
// cannot silently start disclosing it.
type connectionDTO struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Name  string `json:"name"`
	// Description is the administrator's own note; "" when none was written.
	Description string `json:"description"`

	ScopeID    string `json:"scope_id"`
	ScopeLabel string `json:"scope_label"`

	Config map[string]string `json:"config"`

	DefaultDestination database.Destination `json:"default_destination"`

	ForPlatform       bool     `json:"for_platform"`
	ForApps           bool     `json:"for_apps"`
	PlatformAudiences []string `json:"platform_audiences"`
	// IssueTokens lets apps attached here get the connection's access token.
	IssueTokens bool `json:"issue_tokens"`
	// TokenProblem, while IssueTokens is on, is why an app asking right now
	// would be refused — a Graph token carrying permissions beyond files.
	// Checked on every read, because Entra permissions change without Toft
	// hearing. "" when there is none or the switch is off.
	TokenProblem string `json:"token_problem,omitempty"`

	// SecretSet is all a client learns about the stored credential: whether
	// there is one. The form uses it to decide between "paste it" and "leave
	// blank to keep the stored one".
	SecretSet bool `json:"secret_set"`
	// SecretUnreadable is a stored credential this install's keys cannot open —
	// a lost or changed ENCRYPTION_KEY. See credentials.go.
	SecretUnreadable bool `json:"secret_unreadable"`

	// Apps is how many apps are attached, which is what the delete refusal is
	// about and what the page shows before somebody tries.
	Apps int `json:"apps"`
	// AppNames names them, for the confirm in front of the token switch, which
	// hands every one of them the connection's whole reach.
	AppNames []string `json:"app_names"`
	// Placements is how many objects this connection has out in somebody else's
	// system. Zero for every kind that leaves nothing behind.
	Placements int `json:"placements"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func (s *Server) connectionDTO(c database.IntegrationConnection) connectionDTO {
	dto := connectionDTO{
		ID: c.ID, Kind: c.Kind, Name: c.Name, Description: c.Description,
		ScopeID: c.ScopeID, ScopeLabel: c.ScopeLabel,
		Config:             c.Config,
		DefaultDestination: c.DefaultDestination,
		ForPlatform:        c.ForPlatform,
		ForApps:            c.ForApps,
		PlatformAudiences:  c.PlatformAudiences,
		IssueTokens:        c.IssueTokens,
		SecretSet:          c.Secret != "",
		SecretUnreadable:   s.credentialUnreadable(c.Secret),
		CreatedAt:          c.CreatedAt.Format(timeLayout),
		UpdatedAt:          c.UpdatedAt.Format(timeLayout),
	}
	if k := appint.Lookup(c.Kind); k != nil {
		dto.Title = k.Title
	}
	// A kind whose pasted scope carries its credential is shown without it —
	// including a row saved before the split, which still holds the whole URL.
	if _, ok := s.transports[c.Kind].(integrations.ScopeSplitter); ok {
		dto.ScopeID = scopeForDisplay(dto.ScopeID)
	}
	dto.AppNames = []string{}
	if apps, err := s.db.AppsUsingConnection(c.ID); err == nil {
		dto.Apps = len(apps)
		for _, a := range apps {
			dto.AppNames = append(dto.AppNames, a.AppName)
		}
	}
	if n, err := s.db.CountPlacements(c.ID); err == nil {
		dto.Placements = n
	}
	if c.IssueTokens {
		dto.TokenProblem = s.tokenProblem(c)
	}
	return dto
}

// tokenProblem asks the connection's issuer whether a token minted now could
// be handed to an app, and answers only the refusal an administrator fixes in
// the provider's console: a token too wide. A network failure on a page load is
// not news about the connection.
func (s *Server) tokenProblem(c database.IntegrationConnection) string {
	issuer, ok := s.transports[c.Kind].(integrations.Issuer)
	if !ok {
		return ""
	}
	target, err := s.connectionTarget(c.ID, database.Destination{})
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var wide *integrations.TokenTooWideError
	if err := issuer.CheckToken(ctx, target); errors.As(err, &wide) {
		return err.Error()
	}
	return ""
}

const timeLayout = "2006-01-02T15:04:05Z07:00"

// ------------------------------------------------------------------ routes

// handleListConnections is the Integrations page's data: the catalog and what
// has been connected from it.
//
// The catalog rides along rather than living on a route of its own, because a
// page that renders "add a connection" needs both in the same paint and two
// requests to draw one list is two chances to draw it half-populated.
func (s *Server) handleListConnections(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	rows, err := s.db.ListConnections()
	if err != nil {
		log.Printf("integrations: listing connections: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	out := make([]connectionDTO, 0, len(rows))
	for _, c := range rows {
		out = append(out, s.connectionDTO(c))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"catalog":     appint.Known,
		"connections": out,
	})
}

type connectionRequest struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Description is optional on create; on update, absent keeps the stored
	// one and "" clears it — it is a note, not a credential.
	Description *string `json:"description"`
	// ScopeID is written on create and ignored on update. Changing a scope
	// under live attachments re-points every app on it silently; a new scope is
	// a new connection.
	ScopeID string `json:"scope_id"`

	// Config and Credentials are the kind's declared fields, split by whether
	// the descriptor says the field is secret. A field the kind does not
	// declare is refused rather than stored, so a connection cannot accumulate
	// keys nothing reads.
	Config      map[string]string `json:"config"`
	Credentials map[string]string `json:"credentials"`

	DefaultDestinationID    *string `json:"default_destination_id,omitempty"`
	DefaultDestinationLabel *string `json:"default_destination_label,omitempty"`

	ForPlatform       *bool     `json:"for_platform,omitempty"`
	ForApps           *bool     `json:"for_apps,omitempty"`
	PlatformAudiences *[]string `json:"platform_audiences,omitempty"`
	// IssueTokens is accepted only for a kind that issues tokens.
	IssueTokens *bool `json:"issue_tokens,omitempty"`
}

// tokensNotOffered refuses issue_tokens on a kind with no token action: a
// switch that turns on nothing is a setting somebody will believe in.
func tokensNotOffered(w http.ResponseWriter, kind *appint.Kind, req connectionRequest) bool {
	if req.IssueTokens == nil || !*req.IssueTokens || kind.IssuesTokens() {
		return false
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error": kind.Title + " connections do not hand out access tokens — only SharePoint, " +
			"OneDrive and Google Drive do",
		"code": "tokens_not_offered",
		"hint": "leave issue_tokens out for a " + kind.Title + " connection"})
	return true
}

// handleCreateConnection connects one integration.
//
// License-gated on the *creation* and never on honouring one already created,
// the same shape as publishing an app: what a license withholds is the ability
// to configure another, not the operation of what is configured.
func (s *Server) handleCreateConnection(w http.ResponseWriter, r *http.Request) {
	p := s.adminPrincipal(w, r)
	if p == nil {
		return
	}
	if s.refusedUnlicensed(w, r, entitlements.FeatureIntegrations) {
		return
	}

	var req connectionRequest
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
	if strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "give this connection a name — it is how people tell two of the same kind apart",
			"hint":  `send a name, as "name": "Finance team"`})
		return
	}

	description, err := connectionDescription(req.Description)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "shorten the description, or leave it out"})
		return
	}

	config, secrets, err := splitCredentialFields(kind, req.Config, req.Credentials, true)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "send only the config and credentials fields its kind lists — the catalog in GET /api/v1/admin/integrations/connections names them"})
		return
	}
	// A scope that carries its own credential is split before anything else
	// sees it: the clear half is the scope, the rest is sealed with the kind's
	// secret fields and never stored, shown or audited in the clear.
	scope := strings.TrimSpace(req.ScopeID)
	if sp, ok := s.transports[kind.Name].(integrations.ScopeSplitter); ok {
		clear, inScope, err := sp.SplitScope(scope)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": err.Error(), "code": "scope_refused",
				"hint": "correct scope_id, or the credential it is checked with, and create the connection again"})
			return
		}
		scope = clear
		for k, v := range inScope {
			secrets[k] = v
		}
	}
	sealed, err := s.sealCredentials(secrets)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	conn := &database.IntegrationConnection{
		ID:                "iconn_" + uuid.NewString(),
		Kind:              kind.Name,
		Name:              strings.TrimSpace(req.Name),
		Description:       description,
		ScopeID:           scope,
		Config:            config,
		Secret:            sealed,
		ForPlatform:       len(kind.PlatformAudiences) > 0,
		ForApps:           true,
		PlatformAudiences: append([]string(nil), kind.PlatformAudiences...),
	}
	if tokensNotOffered(w, kind, req) {
		return
	}
	applyConnectionSwitches(conn, kind, req)
	teamsBotAudienceRefusal(conn, true)

	// The scope is proved here, while somebody is looking at the form. An
	// operator whose receiver is on a private address, or whose site the
	// credential cannot read, learns it now rather than at 2am inside a queue
	// worker.
	if v, ok := s.transports[kind.Name].(integrations.Verifier); ok {
		label, err := v.Verify(r.Context(), integrations.Target{
			Connection: *conn, Credentials: secrets})
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": err.Error(), "code": "scope_refused",
				"hint": "correct scope_id, or the credential it is checked with, and create the connection again"})
			return
		}
		conn.ScopeLabel = label
	}
	if conn.ScopeLabel == "" {
		conn.ScopeLabel = conn.ScopeID
	}

	if err := s.db.SaveConnection(conn, p.ID, requestMeta(r.Context())); err != nil {
		if isDuplicateKey(err) {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "there is already a " + kind.Title + " connection called " +
					strconv.Quote(conn.Name), "code": "name_taken"})
			return
		}
		log.Printf("integrations: creating a connection: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusCreated, s.connectionDTO(*conn))
}

// handleUpdateConnection edits a connection's name, its credential and its two
// switches. Never its scope — see the migration for why that is a rule.
func (s *Server) handleUpdateConnection(w http.ResponseWriter, r *http.Request) {
	p := s.adminPrincipal(w, r)
	if p == nil {
		return
	}
	conn, kind, ok := s.connectionParam(w, r)
	if !ok {
		return
	}
	// The license gates *new* configuration. Editing what is already there is
	// not refused: a connection an install is relying on must not become
	// unmaintainable because a term lapsed, and switching one off has to keep
	// working whatever a license says.
	var req connectionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body", "hint": badBodyHint})
		return
	}
	if strings.TrimSpace(req.Name) != "" {
		conn.Name = strings.TrimSpace(req.Name)
	}
	if req.Description != nil {
		description, err := connectionDescription(req.Description)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "shorten the description, or leave it out"})
			return
		}
		conn.Description = description
	}
	config, secrets, err := splitCredentialFields(kind, req.Config, req.Credentials, false)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "send only the config and credentials fields its kind lists — the catalog in GET /api/v1/admin/integrations/connections names them"})
		return
	}
	for k, v := range config {
		conn.Config[k] = v
	}
	if len(secrets) > 0 {
		// A partial credential update would leave the other fields as whatever
		// was there, which is a state nobody asked for; a kind's secret fields
		// are rewritten together or not at all.
		sealed, err := s.sealCredentials(secrets)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		conn.Secret = sealed
	} else {
		// Empty keeps the stored one, which is what makes the field write-only.
		// There is deliberately no way to say "clear it" by sending "": clearing
		// a credential is what deleting the connection is for, and an empty
		// string is far more often a form that did not know to omit the field.
		conn.Secret = ""
	}
	if tokensNotOffered(w, kind, req) {
		return
	}
	applyConnectionSwitches(conn, kind, req)
	if msg := teamsBotAudienceRefusal(conn, false); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg, "code": "no_default_destination", "hint": "send default_destination_id with one of GET /api/v1/admin/integrations/connections/" + conn.ID + "/destinations"})
		return
	}

	if err := s.db.SaveConnection(conn, p.ID, requestMeta(r.Context())); err != nil {
		if isDuplicateKey(err) {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "there is already a " + kind.Title + " connection called " +
					strconv.Quote(conn.Name), "code": "name_taken"})
			return
		}
		log.Printf("integrations: updating a connection: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, s.connectionDTO(*conn))
}

// handleDeleteConnection removes a connection, and refuses while apps are on it.
//
// The trusted-sources shape: **disable is immediate and unrefusable, remove is
// refused and names what is in the way.** Switching `for_apps` off stops every
// send through this connection on the next gate check, which is what somebody
// in an incident actually needs; deleting the row while apps point at it would
// leave those apps with an attachment naming nothing.
func (s *Server) handleDeleteConnection(w http.ResponseWriter, r *http.Request) {
	p := s.adminPrincipal(w, r)
	if p == nil {
		return
	}
	conn, _, ok := s.connectionParam(w, r)
	if !ok {
		return
	}
	apps, err := s.db.AppsUsingConnection(conn.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if len(apps) > 0 {
		names := make([]string, 0, len(apps))
		for _, a := range apps {
			name := a.AppName
			if name == "" {
				name = a.AppSlug
			}
			names = append(names, name)
		}
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "this connection is attached to " + strings.Join(names, ", ") +
				" — detach it from those apps first, or switch it off, which takes effect now",
			"code": "connection_in_use",
			"hint": `send {"for_apps": false} to PATCH /api/v1/admin/integrations/connections/` + conn.ID + ` to stop it now, or DELETE /api/v1/apps/{appID}/integrations/` + conn.Kind + ` for each app named`})
		return
	}
	if err := s.db.DeleteConnection(conn.ID, p.ID, requestMeta(r.Context())); err != nil {
		log.Printf("integrations: deleting a connection: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": true})
}

// handleTestConnection sends the administrator's own diagnostic.
//
// AudienceTest, which belongs to no switch: a test that goes quiet because
// notifications are off tests nothing and reads as a broken integration
// (ADR §24).
func (s *Server) handleTestConnection(w http.ResponseWriter, r *http.Request) {
	p := s.adminPrincipal(w, r)
	if p == nil {
		return
	}
	conn, kind, ok := s.connectionParam(w, r)
	if !ok {
		return
	}
	switch kind.Name {
	case appint.IntegrationEmail:
		// Mail's connection is the install's relay, and the relay's own test
		// button is on the Email settings page where it is configured. A second
		// one here would need a recipient this form does not ask for, and would
		// prove the same thing about a different half of the same setup.
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "email is tested on the Email settings page, where the relay is configured — " +
				"and per app, from that app's Integrations tab",
			"code": "not_testable_here",
			"hint": "POST /api/v1/admin/email-settings/test tests the relay, and POST /api/v1/apps/{appID}/integrations/test an app's mail"})
		return

	case appint.IntegrationSlack:
		if s.slack == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "Slack is not available on this install",
				"hint":  "use a connection of another kind, or ask the install's operator to set Slack up"})
			return
		}
		if err := s.slack.PostTest(r.Context(), conn.ID); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
	default:
		// Every other kind proves itself the same way an app would: a real send
		// through the real transport to the connection's default destination.
		target, err := s.connectionTarget(conn.ID, conn.DefaultDestination)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "GET /api/v1/admin/integrations/connections shows whether the connection still exists and can read its secret — save its credentials again if not"})
			return
		}
		transport := s.transports[kind.Name]
		if transport == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": kind.Title + " is not available on this install",
				"hint":  "use a connection of another kind, or ask the install's operator to set " + kind.Title + " up"})
			return
		}
		who := "an administrator"
		if u, err := s.db.GetUser(p.ID); err == nil && u != nil && u.Name != "" {
			who = u.Name
		}
		msg := integrations.Send{
			Action:   kind.Actions[0].Name,
			Text:     "Toft test message, sent by " + who + ". If you can read this, this connection works.",
			Title:    "Toft test",
			Severity: "info",
			DedupKey: "toft-test",
			AppName:      "Toft",
			AppURL:       s.appLinkBase(),
			FromPlatform: true,
		}
		// A file-only kind is tested by the one thing it can do: a one-line
		// text file in the default destination.
		if kind.Actions[0].Carries == appint.CarriesFile {
			f := testFile(msg.Text)
			msg.File = &f
			msg.Files = []appint.File{f}
		}
		res, err := transport.Send(r.Context(), target, msg)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		// The test file is a file this platform put in somebody's library like
		// any other (§72), so it is a placement too: one with no app and no
		// send, which is what it is.
		if res.Placement != nil {
			if err := s.db.InsertPlacement(placementRow(kind.Name, target, *res.Placement)); err != nil {
				log.Printf("integrations: recording the file connection %s's test left: %v",
					conn.ID, err)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"sent": true})
}

// handleConnectionDestinations lists what a connection can reach.
//
// **404 for a kind that does not enumerate, never an empty list.** An empty
// list reads as "none configured yet", which is a different and wrong thing to
// say about a welded webhook that has exactly one destination by construction.
func (s *Server) handleConnectionDestinations(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	conn, kind, ok := s.connectionParam(w, r)
	if !ok {
		return
	}
	s.writeDestinations(w, r, conn, kind)
}

func (s *Server) writeDestinations(w http.ResponseWriter, r *http.Request,
	conn *database.IntegrationConnection, kind *appint.Kind) {

	if !kind.Enumerates {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": kind.Title + " has one destination, fixed when the connection was made — " +
				"there is nothing to choose from",
			"code": "welded",
			"hint": "nothing to choose — GET /api/v1/admin/integrations/connections shows its one destination as default_destination"})
		return
	}
	lister, ok := s.transports[kind.Name].(integrations.Enumerator)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": kind.Title + " is not available on this install",
			"hint":  "use a connection of another kind, or ask the install's operator to set " + kind.Title + " up"})
		return
	}
	target, err := s.connectionTarget(conn.ID, database.Destination{})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "GET /api/v1/admin/integrations/connections shows whether the connection still exists and can read its secret — save its credentials again if not"})
		return
	}
	list, err := lister.Destinations(r.Context(), target)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"label":        kind.DestinationLabel,
		"destinations": list,
	})
}

// handleConnectionPlacements is what this connection has out there.
//
// The whole of "detach reaches nothing already written" as something a person
// can act on. The platform will not issue bulk deletes inside an organization's
// document store on the strength of rows it wrote itself — the first time it
// removed a file somebody had edited afterwards would be the last time anybody
// attached the kind — so what it does instead is make the list readable and
// leave the decision with a human.
func (s *Server) handleConnectionPlacements(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	conn, _, ok := s.connectionParam(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.db.ListPlacements(conn.ID, limit)
	if err != nil {
		log.Printf("integrations: listing placements: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"placements": rows})
}

// ----------------------------------------------------------------- helpers

// connectionParam resolves {connID} and its kind, or answers for itself.
func (s *Server) connectionParam(w http.ResponseWriter, r *http.Request) (
	*database.IntegrationConnection, *appint.Kind, bool) {

	conn, err := s.db.GetConnection(chi.URLParam(r, "connID"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return nil, nil, false
	}
	if conn == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such connection", "hint": "GET /api/v1/admin/integrations/connections lists the connections by id"})
		return nil, nil, false
	}
	kind := appint.Lookup(conn.Kind)
	if kind == nil {
		// A row naming a kind this build does not have. It can only come from a
		// downgrade, and saying so beats pretending the row is broken.
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "this connection is a " + strconv.Quote(conn.Kind) +
				", which this version of Toft does not offer",
			"code": "unknown_kind"})
		return nil, nil, false
	}
	return conn, kind, true
}

// splitCredentialFields sorts what an administrator typed into the clear half
// and the secret half, by what the *kind* declares.
//
// A field the kind does not declare is refused rather than stored. A connection
// that accumulated keys nothing reads would be a credential store with no
// inventory, which is precisely what secrets.go's registry exists to prevent.
// maxConnectionDescription bounds an administrator's note about a connection.
const maxConnectionDescription = 1000

// connectionDescription trims and bounds a description. Nil is no description.
func connectionDescription(v *string) (string, error) {
	if v == nil {
		return "", nil
	}
	d := strings.TrimSpace(*v)
	if n := len([]rune(d)); n > maxConnectionDescription {
		return "", errors.New("a connection's description is at most " +
			strconv.Itoa(maxConnectionDescription) + " characters — this one is " + strconv.Itoa(n))
	}
	return d, nil
}

// scopeForDisplay drops a scope's query string and fragment.
func scopeForDisplay(scope string) string {
	if i := strings.IndexAny(scope, "?#"); i >= 0 {
		return scope[:i]
	}
	return scope
}

func splitCredentialFields(kind *appint.Kind, config, credentials map[string]string,
	creating bool) (map[string]string, map[string]string, error) {

	declared := map[string]appint.Credential{}
	for _, c := range kind.Credentials {
		declared[c.Key] = c
	}
	for key := range config {
		c, ok := declared[key]
		if !ok || c.Secret {
			return nil, nil, fieldError(kind, key)
		}
	}
	for key := range credentials {
		c, ok := declared[key]
		if !ok || !c.Secret {
			return nil, nil, fieldError(kind, key)
		}
	}

	outConfig := map[string]string{}
	outSecret := map[string]string{}
	for _, c := range kind.Credentials {
		value := strings.TrimSpace(config[c.Key])
		if c.Secret {
			value = strings.TrimSpace(credentials[c.Key])
		}
		if value == "" {
			if creating && c.Required {
				return nil, nil, errRequiredField(kind, c)
			}
			continue
		}
		if c.Secret {
			outSecret[c.Key] = value
		} else {
			outConfig[c.Key] = value
		}
	}
	return outConfig, outSecret, nil
}

func fieldError(kind *appint.Kind, key string) error {
	return &badFieldError{kind: kind.Title, key: key}
}

func errRequiredField(kind *appint.Kind, c appint.Credential) error {
	return &missingFieldError{kind: kind.Title, label: c.Label}
}

type badFieldError struct{ kind, key string }

func (e *badFieldError) Error() string {
	return strconv.Quote(e.key) + " is not a field a " + e.kind + " connection has"
}

type missingFieldError struct{ kind, label string }

func (e *missingFieldError) Error() string {
	return "a " + e.kind + " connection needs its " + e.label
}

// applyConnectionSwitches applies the parts of a request that are switches,
// keeping every audience inside the kind's own list.
//
// A connection cannot claim an audience its kind does not serve: PagerDuty
// carries the app audience and never the admin one, because paging the
// operators about a publisher request is how a channel earns a filter rule, and
// that is a property of the kind rather than something a form may override.
func applyConnectionSwitches(conn *database.IntegrationConnection, kind *appint.Kind,
	req connectionRequest) {

	if req.ForPlatform != nil {
		conn.ForPlatform = *req.ForPlatform && len(kind.PlatformAudiences) > 0
	}
	if req.ForApps != nil {
		conn.ForApps = *req.ForApps
	}
	if req.IssueTokens != nil {
		conn.IssueTokens = *req.IssueTokens && kind.IssuesTokens()
	}
	if req.PlatformAudiences != nil {
		kept := []string{}
		for _, a := range *req.PlatformAudiences {
			if kind.Serves(a) {
				kept = append(kept, a)
			}
		}
		conn.PlatformAudiences = kept
	}
	if req.DefaultDestinationID != nil {
		conn.DefaultDestination.ID = strings.TrimSpace(*req.DefaultDestinationID)
	}
	if req.DefaultDestinationLabel != nil {
		conn.DefaultDestination.Label = strings.TrimPrefix(
			strings.TrimSpace(*req.DefaultDestinationLabel), "#")
	}
}

// adminOnly is the 404-to-everyone-else check these routes share.
func (s *Server) adminOnly(w http.ResponseWriter, r *http.Request) bool {
	return s.adminPrincipal(w, r) != nil
}

func (s *Server) adminPrincipal(w http.ResponseWriter, r *http.Request) *auth.Principal {
	p := auth.GetPrincipal(r.Context())
	if p == nil || !p.IsAdmin() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "check the method and the path; nothing was attempted"})
		return nil
	}
	return p
}
