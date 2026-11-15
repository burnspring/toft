package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"toft/internal/auth"
	"toft/internal/database"
)

// The app's one JSON document.
//
// What it is for: the handful of values that *are* an internal tool's
// configuration — the chosen period, a column layout, the id of the last row
// processed — which have to survive a restart and a redeploy and belong to
// nobody's session. Scratch is wiped on every start (ADR §12b); a mapped
// storage resource is durable and is an administrator's separate decision per
// app; a database of the app's own is a decision nobody wants to make for forty
// rows of settings. This is the gap between them, and deliberately nothing more.
//
// What it is not: a store. One document per app, no keys, no listing, no
// queries, and a cap small enough that nobody mistakes it for one — an app that
// outgrows it has outgrown the feature rather than hit a limit worth raising.
// The refusal says so and names what to use instead, because "413" on its own
// sends somebody to write their own paging.
//
// Like everything else under /api/v1/app, no route here takes an app id: the
// token is the app, so no handler calls auth.Authorize and none can have an
// IDOR. The two platform-side routes at the bottom of this file are the
// exception that proves it — they take an id, so they authorize, and they never
// serve the document itself.

// maxAppDataBytes bounds one document.
//
// A settings document, at a size where the choice between this and a real store
// is made once and obviously, rather than discovered at 3 MiB by an app that had
// been treating a platform table as its database. It is compiled in rather than
// an administrator's setting on purpose: raising it would not fix the app that
// hits it, and every install has to behave the same way here or an app written
// against one silently breaks on another.
const maxAppDataBytes = 256 << 10

// appDataResponse is the one shape this surface answers with. Document included
// only on a read — an echo of what was just written is bytes nobody asked for,
// and a delete has nothing to echo.
type appDataResponse struct {
	Data      json.RawMessage `json:"data,omitempty"`
	Version   int64           `json:"version"`
	UpdatedAt interface{}     `json:"updated_at"`
	Bytes     int             `json:"bytes"`
	Deleted   *bool           `json:"deleted,omitempty"`
}

// handleAppDataGet reads the document back.
//
// 200 with `"data": null` when the app has never written one, rather than a
// 404. "Nothing yet" is the state every app starts in and branches on at
// startup; making it an error means every app's first call is inside a
// try/except, and the one that forgets crashes on its first boot after deploy.
func (s *Server) handleAppDataGet(w http.ResponseWriter, r *http.Request) {
	token := appTokenFromContext(r.Context())
	if token == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no app token"})
		return
	}
	data, err := s.db.GetAppData(token.Token.AppID)
	if err != nil {
		log.Printf("app data: read %s: %v", token.Token.AppID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if data == nil {
		writeJSON(w, http.StatusOK, appDataResponse{
			Data: json.RawMessage("null"), UpdatedAt: nil})
		return
	}
	setDataETag(w, data.Version)
	writeJSON(w, http.StatusOK, appDataResponse{
		Data:      json.RawMessage(data.Document),
		Version:   data.Version,
		UpdatedAt: data.UpdatedAt,
		Bytes:     data.Bytes(),
	})
}

// handleAppDataPut replaces the document.
//
// The body *is* the document — not a wrapper with the document inside a field.
// A wrapper would make `PUT {"data": …}` and a document whose own top-level key
// happened to be `data` two different things that look identical in a log, and
// the precondition has a header for it precisely so the body can stay the app's.
func (s *Server) handleAppDataPut(w http.ResponseWriter, r *http.Request) {
	token := appTokenFromContext(r.Context())
	if token == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no app token"})
		return
	}
	ifVersion, err := parseIfMatch(r.Header.Get("If-Match"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": err.Error(), "code": "bad_precondition", "hint": `send If-Match with the version a read returned, like If-Match: "3", or leave it out to write regardless`})
		return
	}

	// One byte past the cap, so a document exactly at it is accepted and the
	// first byte over is refused by the reader rather than by a length check
	// that has already buffered whatever was sent.
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAppDataBytes+1))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]interface{}{
			"error": "this document is larger than " + strconv.Itoa(maxAppDataBytes/1024) +
				" KiB, which is the whole of what app data holds. It is for settings and " +
				"small state; anything bigger belongs in storage mapped to this app, or in " +
				"a database of the app's own.",
			"code":  "too_large",
			"hint":  appDataTooLargeHint,
			"limit": maxAppDataBytes,
		})
		return
	}
	// MaxBytesReader(n) hands back exactly n bytes without complaint, so the
	// one byte the reader allowed past the cap is refused here; the reader's
	// own refusal is for the second byte over, where it has already stopped
	// buffering.
	if len(raw) > maxAppDataBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]interface{}{
			"error": "this document is larger than " + strconv.Itoa(maxAppDataBytes/1024) +
				" KiB, which is the whole of what app data holds. It is for settings and " +
				"small state; anything bigger belongs in storage mapped to this app, or in " +
				"a database of the app's own.",
			"code":  "too_large",
			"hint":  appDataTooLargeHint,
			"limit": maxAppDataBytes,
		})
		return
	}
	document, msg := validateAppDocument(raw)
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": msg, "code": "bad_document",
			"hint": "send one JSON object, as UTF-8 text — a dict in Python, an object in JS, a named list in R"})
		return
	}

	data, err := s.db.PutAppData(token.Token.AppID, document, ifVersion)
	if errors.Is(err, database.ErrAppDataConflict) {
		s.writeDataConflict(w, token.Token.AppID)
		return
	}
	if err != nil {
		log.Printf("app data: write %s: %v", token.Token.AppID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	setDataETag(w, data.Version)
	writeJSON(w, http.StatusOK, appDataResponse{
		Version: data.Version, UpdatedAt: data.UpdatedAt, Bytes: data.Bytes()})
}

// handleAppDataDelete puts the app back to never-having-written-one.
//
// Not "write {}" — an app that clears its settings and one that saved an empty
// object mean different things, and the app is entitled to tell them apart on
// its next read.
func (s *Server) handleAppDataDelete(w http.ResponseWriter, r *http.Request) {
	token := appTokenFromContext(r.Context())
	if token == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no app token"})
		return
	}
	ifVersion, err := parseIfMatch(r.Header.Get("If-Match"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": err.Error(), "code": "bad_precondition", "hint": `send If-Match with the version a read returned, like If-Match: "3", or leave it out to delete regardless`})
		return
	}

	deleted, err := s.db.DeleteAppData(token.Token.AppID, ifVersion)
	if errors.Is(err, database.ErrAppDataConflict) {
		s.writeDataConflict(w, token.Token.AppID)
		return
	}
	if err != nil {
		log.Printf("app data: delete %s: %v", token.Token.AppID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, appDataResponse{UpdatedAt: nil, Deleted: &deleted})
}

// writeDataConflict reports a precondition that no longer holds, with the
// version that is actually stored.
//
// 412 rather than 409: the request was fine and the *precondition* failed,
// which is the distinction the status code exists for. Carrying the current
// version turns the app's recovery into one round trip instead of two, and the
// SDKs' `update()` loop is built on exactly that.
func (s *Server) writeDataConflict(w http.ResponseWriter, appID string) {
	var current int64
	if data, err := s.db.GetAppData(appID); err == nil && data != nil {
		current = data.Version
	}
	writeJSON(w, http.StatusPreconditionFailed, map[string]interface{}{
		"error": "the document changed since the version you named — re-read it and " +
			"apply your change again",
		"code":    "version_conflict",
		"hint":    "read the document again, reapply your change to it, and write with the version you just read — the SDK's update() loop does this",
		"version": current,
	})
}

func setDataETag(w http.ResponseWriter, version int64) {
	w.Header().Set("ETag", `"`+strconv.FormatInt(version, 10)+`"`)
}

// validateAppDocument checks the two things the platform is entitled to check,
// and returns the bytes to store. Everything else about the document is the
// app's business and is never read here.
func validateAppDocument(raw []byte) (string, string) {
	doc := bytes.TrimSpace(raw)
	if len(doc) == 0 {
		return "", "the request body is the document itself, and it is empty. " +
			"Send a JSON object, or DELETE to clear what is stored."
	}
	// A JSON *object*, not any JSON value. Two reasons, and the second is the
	// load-bearing one. An object is the only shape that can gain a field later
	// without every reader of it changing. And a stored `null` would be
	// indistinguishable on read from having written nothing at all — which is a
	// state this surface promises to keep tellable apart.
	if doc[0] != '{' {
		return "", "app data holds one JSON object. An array or a bare value has " +
			"nowhere to grow a field later, and a stored null would be " +
			"indistinguishable from never having saved anything — wrap it, e.g. " +
			`{"items": […]}.`
	}
	if !json.Valid(doc) {
		return "", "this is not valid JSON"
	}
	// Postgres would refuse invalid UTF-8 with an error nobody can act on, and a
	// NUL byte outright. Named here instead, while the caller can still tell
	// which field they built from bytes rather than text.
	if !utf8.Valid(doc) {
		return "", "this document is not valid UTF-8 — JSON is text, and raw bytes " +
			"belong in a base64 string or on storage mapped to this app"
	}
	return string(doc), ""
}

// parseIfMatch reads the caller's precondition. nil means there was none, which
// is last-write-wins — the right default for the single-writer app that is the
// common case, and a trap only for an app that already knows it has two writers.
//
// `0` means "only if nothing is stored", because absent is version 0. That is
// why create-only needs no separate route and no separate flag.
func parseIfMatch(header string) (*int64, error) {
	raw := strings.TrimSpace(header)
	if raw == "" {
		return nil, nil
	}
	if raw == "*" {
		// Standard HTTP, and meaningless here: this resource always exists as
		// far as the app is concerned — it is either a document or version 0.
		// Accepting `*` would mean inventing a third state to distinguish.
		return nil, errors.New(`If-Match takes the version a read returned, e.g. If-Match: "3" ` +
			`(or "0" for "only if nothing is stored"). "*" has no meaning here.`)
	}
	if strings.HasPrefix(raw, "W/") {
		return nil, errors.New("If-Match takes the version a read returned; a weak validator " +
			"cannot express a precondition on a document")
	}
	raw = strings.Trim(raw, `"`)
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return nil, errors.New(`If-Match takes the version a read returned, e.g. If-Match: "3"`)
	}
	return &v, nil
}

// --- The platform-side pair ---
//
// These take an app id, so the standing rule is back in force: both authorize,
// and both scope by the app they resolved.
//
// Neither one serves the document. Not because reading it would be a boundary
// crossing — an owner can deploy code into the app that prints it, so withholding
// it from them protects nothing — but because *displaying* app-owned state is a
// second, unversioned editor for it, and this feature deliberately has one
// writer: the app. What an owner needs from the platform is the fact that
// something is stored, how big it is, when it changed, and a way to throw it
// away.

// handleAppDataMeta answers "is there anything in there, and when did it change".
func (s *Server) handleAppDataMeta(w http.ResponseWriter, r *http.Request) {
	app, ok := s.appForData(w, r, auth.ActionRead)
	if !ok {
		return
	}
	data, err := s.db.GetAppData(app.ID)
	if err != nil {
		log.Printf("app data: read %s: %v", app.ID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if data == nil {
		writeJSON(w, http.StatusOK, appDataResponse{UpdatedAt: nil})
		return
	}
	writeJSON(w, http.StatusOK, appDataResponse{
		Version: data.Version, UpdatedAt: data.UpdatedAt, Bytes: data.Bytes()})
}

// handleAppDataReset throws the document away on the app's behalf.
//
// The escape hatch, and the reason the platform side exists at all: an app that
// writes a document its own startup code cannot read has wedged itself in a way
// no redeploy fixes, because the whole point of this store is that a redeploy
// does not touch it. Without this, the only way out is deleting the app.
//
// Audited, unlike anything the app does to its own document. The distinction is
// who acted: an app writing its settings is the app working, and a person
// reaching in and erasing them is an event somebody will later ask about.
func (s *Server) handleAppDataReset(w http.ResponseWriter, r *http.Request) {
	app, ok := s.appForData(w, r, auth.ActionEditContent)
	if !ok {
		return
	}
	// The reset erases state the app runs with, and a lock promises to leave
	// what an app contains alone (security review 2026-11-15, LSA-2).
	if s.refuseIfLocked(w, app) {
		return
	}
	// The pre-read is for the audit row's before-state, and it is allowed to
	// fail: a reset that happened is audited whether or not this read did.
	before, err := s.db.GetAppData(app.ID)
	if err != nil {
		log.Printf("app data: reading %s before reset: %v", app.ID, err)
	}

	deleted, err := s.db.DeleteAppData(app.ID, nil)
	if err != nil {
		log.Printf("app data: reset %s: %v", app.ID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if deleted {
		meta := requestMeta(r.Context())
		actor := ""
		if p := auth.GetPrincipal(r.Context()); p != nil {
			actor = p.ID
		}
		// The version and the size, never the content. An audit row is read by
		// people who are not the app's owner, and copying app-owned state into
		// it would put it somewhere this design never said it would go.
		entry := map[string]interface{}{}
		if before != nil {
			entry["version"] = before.Version
			entry["bytes"] = before.Bytes()
		}
		if err := s.db.WriteAuditRow(database.AuditEntry{
			ActorType: database.PrincipalTypeUser, ActorID: actor,
			Action:     "app.data_reset",
			TargetType: "app", TargetID: app.ID,
			Before:    jsonString(entry),
			RequestID: meta.RequestID, IP: meta.IP, UserAgent: meta.UserAgent, APIKeyID: meta.APIKeyID,
		}); err != nil {
			// Best-effort, like every other audit row written after the fact:
			// refusing to report a completed reset helps nobody.
			log.Printf("app data: audit reset of %s: %v", app.ID, err)
		}
	}
	writeJSON(w, http.StatusOK, appDataResponse{UpdatedAt: nil, Deleted: &deleted})
}

// appForData resolves the app in the URL and authorizes against it. 404 for an
// app this caller may not read, which is the standing rule for an unreadable
// object rather than a special case here.
func (s *Server) appForData(w http.ResponseWriter, r *http.Request, action auth.Action) (*database.App, bool) {
	app, ok := s.appOr404(w, chi.URLParam(r, "appID"))
	if !ok {
		return nil, false
	}
	p := auth.GetPrincipal(r.Context())
	if d := auth.Authorize(p, action, objectFromApp(app), s.db); !d.Allow {
		writeAuthzError(w, r, d, false)
		return nil, false
	}
	return app, true
}

// appDataTooLargeHint is too_large's next step on an app data write.
const appDataTooLargeHint = "keep app data to settings and small state; put larger data in storage mapped to the app, or in a database of its own"
