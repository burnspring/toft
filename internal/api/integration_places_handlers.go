package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"toft/internal/appint"
	"toft/internal/database"
	"toft/internal/integrations/teamsbot"
)

// The Teams bot's places and its app package (ADR §72,
// docs/dev-reference.md § App Integrations).
//
// A place is a team or a chat an administrator pasted and the Bot Connector
// answered for — which it does only where the Toft app is installed, so saving
// one is the membership check. Every route here is an administrator's, 404 to
// anyone else, and on keyroutes.go's denied list with the rest of
// /admin/integrations: where a bot may post is a credential's reach.

// placeDTO is a place as the admin page reads it. Reachable is asked of the far
// side when the list is read: a team that has removed the Toft app since it was
// pasted says so here rather than vanishing from the destinations menu without
// a word.
type placeDTO struct {
	database.IntegrationPlace
	Reachable bool   `json:"reachable"`
	Problem   string `json:"problem,omitempty"`
}

// placesConnection is connectionParam, narrowed to the one kind that has places.
func (s *Server) placesConnection(w http.ResponseWriter, r *http.Request) (*database.IntegrationConnection, bool) {
	conn, kind, ok := s.connectionParam(w, r)
	if !ok {
		return nil, false
	}
	if kind.Name != appint.IntegrationTeamsBot {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": kind.Title + " has no places — its far side is asked directly",
			"code":  "no_places",
			"hint":  "use a Teams bot connection's id here — GET /api/v1/admin/integrations/connections lists each connection with its kind"})
		return nil, false
	}
	return conn, true
}

func (s *Server) handleListPlaces(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	conn, ok := s.placesConnection(w, r)
	if !ok {
		return
	}
	target, err := s.connectionTarget(conn.ID, database.Destination{})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "GET /api/v1/admin/integrations/connections shows whether the connection still exists and can read its secret — save its credentials again if not"})
		return
	}
	out := make([]placeDTO, 0, len(target.Places))
	for _, p := range target.Places {
		dto := placeDTO{IntegrationPlace: p, Reachable: true}
		if p.Kind == database.PlaceTeam {
			if _, err := teamsbot.Channels(r.Context(), target, p.ConversationID); err != nil {
				dto.Reachable, dto.Problem = false, err.Error()
			}
		}
		out = append(out, dto)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"places": out})
}

type addPlaceRequest struct {
	Link string `json:"link"`
	// Name is a chat's: Teams does not say what a chat is called.
	Name string `json:"name"`
}

func (s *Server) handleAddPlace(w http.ResponseWriter, r *http.Request) {
	p := s.adminPrincipal(w, r)
	if p == nil {
		return
	}
	conn, ok := s.placesConnection(w, r)
	if !ok {
		return
	}
	var req addPlaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body", "hint": badBodyHint})
		return
	}
	link, err := teamsbot.ParseLink(req.Link, conn.Config["tenant_id"])
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "bad_link", "hint": `copy the channel's or chat's link from Teams and send it as "link"`})
		return
	}
	target, err := s.connectionTarget(conn.ID, database.Destination{})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": "GET /api/v1/admin/integrations/connections shows whether the connection still exists and can read its secret — save its credentials again if not"})
		return
	}
	for _, existing := range target.Places {
		if existing.ConversationID == link.ConversationID {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "that " + existing.Kind + " is already on this connection, as “" +
					existing.Name + "”",
				"code": "place_exists",
				"hint": "GET /api/v1/admin/integrations/connections/" + conn.ID + "/places lists the places already on it"})
			return
		}
	}

	name, err := teamsbot.VerifyPlace(r.Context(), target, link, req.Name)
	if err != nil {
		var apiErr *teamsbot.APIError
		switch {
		case teamsbot.NotInstalled(err):
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "Toft isn't in that " + link.Kind + " — add the Toft app to it in Teams, " +
					"then paste the link again",
				"code": "not_installed",
				"hint": "GET /api/v1/admin/integrations/connections/" + conn.ID + "/teams-app downloads the Toft app to upload in the Teams admin center"})
		case errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound:
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "Teams has no such " + link.Kind + " — check the link",
				"code":  "no_such_place",
				"hint":  "copy the link again in Teams — from this connection's tenant, and for a " + link.Kind + " that still exists"})
		case errors.As(err, &apiErr):
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		default:
			// Not the far side's answer: a chat with no name, or no token.
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "hint": `send the chat's "name" with its link — Teams does not say what a chat is called`})
		}
		return
	}

	place := &database.IntegrationPlace{
		ID: "ipl_" + uuid.NewString(), ConnectionID: conn.ID,
		Kind: link.Kind, ConversationID: link.ConversationID, Name: name,
	}
	if err := s.db.AddPlace(place, p.ID, requestMeta(r.Context())); err != nil {
		if isDuplicateKey(err) {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "that " + link.Kind + " is already on this connection", "code": "place_exists",
				"hint": "GET /api/v1/admin/integrations/connections/" + conn.ID + "/places lists the places already on it"})
			return
		}
		log.Printf("integrations: adding a place: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusCreated, placeDTO{IntegrationPlace: *place, Reachable: true})
}

func (s *Server) handleDeletePlace(w http.ResponseWriter, r *http.Request) {
	p := s.adminPrincipal(w, r)
	if p == nil {
		return
	}
	conn, ok := s.placesConnection(w, r)
	if !ok {
		return
	}
	place, err := s.db.GetPlace(conn.ID, chi.URLParam(r, "placeID"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if place == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such place on this connection", "hint": "GET /api/v1/admin/integrations/connections/" + conn.ID + "/places lists its places by id"})
		return
	}
	apps, isDefault, err := s.db.PlaceInUse(conn.ID, teamsbot.PlacePrefix(*place))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if len(apps) > 0 || isDefault {
		holders := append([]string{}, apps...)
		if isDefault {
			holders = append(holders, "this connection's default destination")
		}
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "“" + place.Name + "” is in use by " + strings.Join(holders, ", ") +
				" — choose another destination there first",
			"code": "place_in_use",
			"hint": "PUT /api/v1/apps/{appID}/integrations/teamsbot/destination moves an app to another place"})
		return
	}
	if err := s.db.DeletePlace(place, p.ID, requestMeta(r.Context())); err != nil {
		log.Printf("integrations: removing a place: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": true})
}

// handleTeamsAppPackage is the Teams app an administrator uploads in the Teams
// admin center. Its manifest id is derived from the connection, so downloading
// it again is the same app, not a second one.
func (s *Server) handleTeamsAppPackage(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	conn, ok := s.placesConnection(w, r)
	if !ok {
		return
	}
	pkg, err := teamsbot.Package(teamsbot.PackageInfo{
		ConnectionID: conn.ID, BotID: conn.ScopeID, Name: conn.Name, WebsiteURL: s.appLinkBase(),
	})
	if err != nil {
		log.Printf("integrations: building the Teams app package: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="toft-teams-app.zip"`)
	_, _ = w.Write(pkg)
}

// teamsBotAudienceRefusal is D9: a Teams bot's admin notices go to the
// connection's default destination, so the `admin` audience without one would
// deliver nothing. On create the audience is dropped quietly — the form has not
// offered a default yet; on update it is refused, because somebody asked for it.
func teamsBotAudienceRefusal(conn *database.IntegrationConnection, creating bool) string {
	if conn.Kind != appint.IntegrationTeamsBot || conn.DefaultDestination.ID != "" {
		return ""
	}
	kept := []string{}
	hadAdmin := false
	for _, a := range conn.PlatformAudiences {
		if a == "admin" {
			hadAdmin = true
			continue
		}
		kept = append(kept, a)
	}
	if !hadAdmin || !conn.ForPlatform {
		return ""
	}
	if creating {
		conn.PlatformAudiences = kept
		return ""
	}
	return "choose a default channel for this bot first — Toft's notices for administrators " +
		"are posted there"
}
