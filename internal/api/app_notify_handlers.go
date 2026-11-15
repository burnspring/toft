package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"toft/internal/appint"
	"toft/internal/database"
	mailer "toft/internal/mail"
)

// Accepting an app-initiated send.
//
// The shape is the same for every route here and the order matters:
//
//  1. Parse and bound the request. Every limit is a refusal that names the
//     limit, never a truncation — an app that silently loses the tail of a
//     report has a report that is wrong in a way nobody notices.
//  2. Gate it (appint.Check), once, in one place.
//  3. Answer an idempotent replay without sending anything.
//  4. Write the ledger row *before* attempting anything, so the 202 is a
//     promise the record can keep.
//  5. Enqueue. A full queue is a 429, never a silent drop: a 202 for a message
//     that was discarded is worse than a refusal, because the app stops
//     watching.
//
// **Two general routes and three published aliases.** `/notify/{kind}` and
// `/deliver/{kind}` are what the catalog gives an app; `/notify/slack`,
// `/notify/slack/file` and `/notify/email` keep working unchanged, permanently,
// because both SDKs call them and the SDK contract is that an app written
// against one release keeps running on the next. They are the same handler
// bodies with the kind supplied rather than read from the URL, which is what
// stops the promise from costing a second implementation to drift from.
//
// `{kind}` is not an app id, so the standing rule that no route under
// /api/v1/app takes one is untouched.

// Limits that are not a kind's own. Everything about the size of a message
// belongs to the catalog (appint.Kind.TextLimit, .FileLimit); what is left here
// is the shape of the request rather than the size of the content.
const (
	emailRecipients  = 100
	emailSubjectMax  = 200
	emailHTMLLimit   = 512 << 10
	emailAttachMax   = 5
	multipartMemory  = 1 << 20
	idempotencyLabel = "Idempotency-Key"
)

// destinationFields are the names an app might use to try to choose where its
// message goes.
//
// **Refused, never ignored.** A silently dropped field is a caller who believes
// they chose the destination, and where an app's messages go is its owners'
// decision, made in the UI from what the connection can actually reach. ADR §25
// is the rule; this map is the enforcement, and it is one map rather than a
// check per kind so a ninth integration cannot forget it.
var destinationFields = map[string]string{
	"channel":     "the channel it posts to",
	"folder":      "the folder its files go to",
	"path":        "where its files go",
	"url":         "the address it posts to",
	"routing_key": "the service it pages",
	"space":       "the space it posts to",
	"site":        "the site it writes to",
	"webhook":     "the receiver it posts to",
	"to_channel":  "its channel",
}

// appNotifyKind resolves the kind for a request: the URL parameter on the
// general routes, or the fixed value the published aliases pass.
func appNotifyKind(r *http.Request, fixed string) (*appint.Kind, bool) {
	name := fixed
	if name == "" {
		name = chi.URLParam(r, "kind")
	}
	k := appint.Lookup(name)
	return k, k != nil
}

func writeUnknownKind(w http.ResponseWriter, r *http.Request, fixed string) {
	name := fixed
	if name == "" {
		name = chi.URLParam(r, "kind")
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error": strconv.Quote(name) + " is not an integration this platform offers",
		"code":  "unknown_kind",
		"hint":  "use one of " + strings.Join(appint.KindNames(), ", ") + " — check the spelling"})
}

// ------------------------------------------------------------ text sends

// handleAppNotify is the general text route: POST /api/v1/app/notify/{kind}.
func (s *Server) handleAppNotify(w http.ResponseWriter, r *http.Request) {
	s.appNotify(w, r, "")
}

// handleAppNotifySlack is POST /api/v1/app/notify/slack, published and kept.
func (s *Server) handleAppNotifySlack(w http.ResponseWriter, r *http.Request) {
	s.appNotify(w, r, appint.IntegrationSlack)
}

// handleAppNotifyEmail is POST /api/v1/app/notify/email, published and kept.
func (s *Server) handleAppNotifyEmail(w http.ResponseWriter, r *http.Request) {
	s.appNotify(w, r, appint.IntegrationEmail)
}

func (s *Server) appNotify(w http.ResponseWriter, r *http.Request, fixed string) {
	token := appTokenFromContext(r.Context())
	if token == nil {
		writeNoAppToken(w)
		return
	}
	kind, ok := appNotifyKind(r, fixed)
	if !ok {
		writeUnknownKind(w, r, fixed)
		return
	}
	action := kind.TextAction()
	if action == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": kind.Title + " does not take a message — see /deliver/" + kind.Name,
			"code":  "wrong_shape",
			"hint":  "send a file to " + kind.Title + " instead — POST /api/v1/app/deliver/" + kind.Name + ", or the SDK's file call"})
		return
	}
	if kind.Name == appint.IntegrationEmail {
		s.appNotifyEmail(w, r, token, kind)
		return
	}

	raw, ok := readNotifyBody(w, r, int64(kind.TextLimit)+1<<16)
	if !ok {
		return
	}
	var body struct {
		Text string `json:"text"`
		// Slack blocks, present only so they can be refused: a block tree can
		// reproduce Toft's own alert formatting field for field, and there
		// is no cheap validator for "does not look like the platform".
		Blocks   any    `json:"blocks"`
		Title    string `json:"title"`
		Severity string `json:"severity"`
		DedupKey string `json:"dedup_key"`
		// Action picks between a kind's text actions where it has more than
		// one — PagerDuty's trigger and resolve are the pair that exists. It
		// names an action on *this* kind and nothing else: it is not a way to
		// reach another integration, and a name the kind does not have is a 400
		// rather than a fall through to the first one.
		Action string `json:"action"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		writeNotifyBadJSON(w)
		return
	}
	if named := strings.TrimSpace(body.Action); named != "" {
		chosen := kind.Action(named)
		if chosen == nil || chosen.Carries != appint.CarriesText {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": strconv.Quote(named) + " is not something " + kind.Title + " does",
				"code":  "unknown_action",
				"hint":  "leave action out, or name one of " + kind.Title + "'s text actions: " + textActionNames(kind)})
			return
		}
		action = chosen
	}
	if refused := refuseDestinationFields(w, kind.Title, raw); refused {
		return
	}
	if body.Blocks != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Slack blocks are not accepted — send mrkdwn in 'text'. Arbitrary " +
				"blocks can reproduce Toft's own alert formatting, which would let " +
				"an app post as the platform",
			"code": "blocks_not_allowed",
			"hint": "remove blocks and put the message in text, as Slack mrkdwn"})
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "'text' is required",
			"hint":  "pass the message as text — it cannot be empty or only whitespace"})
		return
	}
	if len(text) > kind.TextLimit {
		writeNotifyTooLarge(w, fmt.Sprintf("'text' is %d characters and the limit for %s is %d — "+
			"nothing is truncated, so send less or send twice",
			len(text), kind.Title, kind.TextLimit),
			fmt.Sprintf("shorten the text to %d characters, or send it in parts — or put it in storage and send a link", kind.TextLimit))
		return
	}

	if kind.Name == appint.IntegrationPagerDuty && strings.TrimSpace(body.DedupKey) == "" {
		// Refused here rather than by the transport in the worker, where the
		// app would hold a 202 and nobody would be paged.
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "PagerDuty needs a dedup_key — it is what says which incident this is, " +
				"and what closes it later",
			"code": "dedup_key_required",
			"hint": `pass dedup_key, e.g. "dedup_key":"ingest-late", and send the same key with action resolve to close it`})
		return
	}

	digest, size := appint.Digest(text)
	payload := appint.Payload{
		Text:     text,
		Title:    strings.TrimSpace(body.Title),
		Severity: strings.TrimSpace(body.Severity),
		DedupKey: strings.TrimSpace(body.DedupKey),
	}
	s.acceptSend(w, r, token, kind.Capability(action.Name), payload,
		sendRecord{BodySHA256: digest, BodyBytes: size, Body: text})
}

// ------------------------------------------------------------ file sends

// handleAppDeliver is the general file route: POST /api/v1/app/deliver/{kind}.
func (s *Server) handleAppDeliver(w http.ResponseWriter, r *http.Request) {
	s.appDeliver(w, r, "")
}

// handleAppNotifySlackFile is POST /api/v1/app/notify/slack/file, published and
// kept.
func (s *Server) handleAppNotifySlackFile(w http.ResponseWriter, r *http.Request) {
	s.appDeliver(w, r, appint.IntegrationSlack)
}

func (s *Server) appDeliver(w http.ResponseWriter, r *http.Request, fixed string) {
	token := appTokenFromContext(r.Context())
	if token == nil {
		writeNoAppToken(w)
		return
	}
	kind, ok := appNotifyKind(r, fixed)
	if !ok {
		writeUnknownKind(w, r, fixed)
		return
	}
	action := kind.FileAction()
	if action == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": kind.Title + " does not take a file",
			"code":  "wrong_shape",
			"hint":  "send " + kind.Title + " a message instead — POST /api/v1/app/notify/" + kind.Name + ", or the SDK's send call"})
		return
	}
	if kind.Name == appint.IntegrationEmail {
		s.appNotifyEmail(w, r, token, kind)
		return
	}
	if err := parseAppMultipart(w, r, kind); err != nil {
		if tooLarge(w, err) {
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "this endpoint takes multipart/form-data with a 'file' part",
			"hint":  "send the file as a multipart/form-data part named file — the SDK's file call does"})
		return
	}
	for field, what := range destinationFields {
		if r.FormValue(field) != "" {
			writeDestinationRefusal(w, kind.Title, field, what)
			return
		}
	}
	files, err := readFileParts(r, "file", 1, kind.FileLimit)
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
			"error": err.Error(), "code": "too_large", "hint": notifyFilesHint(kind.FileLimit)})
		return
	}
	if len(files) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no 'file' part in the request",
			"hint": "send the file as a multipart/form-data part named file"})
		return
	}

	// **The folder is the attachment's and the name is the app's.** A supplied
	// name is refused rather than sanitised where it carries a separator or a
	// reserved device name: a silently rewritten name is a file the app cannot
	// find again.
	if name := strings.TrimSpace(r.FormValue("name")); name != "" {
		if err := appint.CheckFilename(name); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": err.Error(), "code": "bad_filename",
				"hint": "pass a plain file name — no folder separators, no reserved device names — or leave name out to keep the file's own"})
			return
		}
		files[0].Name = name
	}

	payload := appint.Payload{
		Title:   strings.TrimSpace(r.FormValue("title")),
		Comment: strings.TrimSpace(r.FormValue("comment")),
		Files:   files,
	}
	s.acceptSend(w, r, token, kind.Capability(action.Name), payload, sendRecord{
		Subject:     payload.Title,
		BodySHA256:  appint.DigestBytes(files[0].Content),
		BodyBytes:   len(files[0].Content),
		Attachments: describeFiles(files),
	})
}

// ------------------------------------------------------------------- mail

// appNotifyEmail is mail's own body, and it is its own body for the reason
// internal/notify refuses a Channel interface over mail: every other kind
// resolves one destination from an attachment, and mail computes a recipient
// list the app supplied. Folding it into the general shape would mean a
// recipient field on every kind that has no recipients.
func (s *Server) appNotifyEmail(w http.ResponseWriter, r *http.Request,
	token *database.ResolvedAppToken, kind *appint.Kind) {

	var body struct {
		To      []string `json:"to"`
		CC      []string `json:"cc"`
		BCC     []string `json:"bcc"`
		Subject string   `json:"subject"`
		Text    string   `json:"text"`
		HTML    string   `json:"html"`
		// Refused, not ignored: all three are the platform's identity.
		From    string `json:"from"`
		ReplyTo string `json:"reply_to"`
		Headers any    `json:"headers"`
	}
	var files []appint.File
	var raw []byte

	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		if err := parseAppMultipart(w, r, kind); err != nil {
			if tooLarge(w, err) {
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "this endpoint takes JSON, or multipart/form-data with a 'payload' " +
					"field and 'attachment' parts",
				"hint": "send JSON, or a multipart body with the message as JSON in payload and each file as an attachment part"})
			return
		}
		raw = []byte(r.FormValue("payload"))
		if err := json.Unmarshal(raw, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "the 'payload' field is not valid JSON",
				"hint":  "put the message — to, subject, text — in payload as one JSON object"})
			return
		}
		var err error
		if files, err = readFileParts(r, "attachment", emailAttachMax, kind.FileLimit); err != nil {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
				"error": err.Error(), "code": "too_large", "hint": notifyFilesHint(kind.FileLimit)})
			return
		}
	} else {
		var ok bool
		if raw, ok = readNotifyBody(w, r, int64(kind.TextLimit)+emailHTMLLimit+1<<16); !ok {
			return
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			writeNotifyBadJSON(w)
			return
		}
	}

	if body.From != "" || body.ReplyTo != "" || body.Headers != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "an app does not choose its sender or reply-to — mail goes out as " +
				"\"<App Name>" + mailer.AppDisplaySuffix(s.db.ResolveSenderName()) +
				"\" from this install's address, and replies go " +
				"to the address set on its Integrations tab, or to the app's owner " +
				"(the install's reply-to, for a group-owned app) when none is",
			"code": "sender_not_allowed",
			"hint": "remove from, reply_to and headers from the call; the reply-to address is set on the app's Integrations tab"})
		return
	}
	if refused := refuseDestinationFields(w, kind.Title, raw); refused {
		return
	}

	to, err := normalizeRecipients(body.To, body.CC, body.BCC)
	if err != nil {
		// Its own code — beside no_recipient and too_many_recipients, the
		// three a sender branches on — now with a hint, like its neighbours
		// (BB-012, second 2026-11-03 bash).
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "code": "bad_recipient",
			"retryable": false,
			"hint":      "pass each recipient as one email address, like ana@example.com, or a list of them"})
		return
	}
	subject := mailer.SanitizeSubject(body.Subject)
	switch {
	case len(to.All) == 0:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "at least one recipient is required",
			"code": "no_recipient", "retryable": false,
			"hint": "name who it goes to: pass an address in to (or cc, bcc) — an email has no default recipient"})
		return
	case len(to.All) > emailRecipients:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("%d recipients, and the limit for one message is %d — "+
				"send more messages; every one of them is in the record", len(to.All), emailRecipients),
			"code": "too_many_recipients",
			"hint": fmt.Sprintf("send at most %d recipients in one message, split across several sends", emailRecipients)})
		return
	case subject == "":
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "'subject' is required",
			"hint":  "pass a subject — an email cannot go without one"})
		return
	case len(subject) > emailSubjectMax:
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
			"error": fmt.Sprintf("the subject is %d characters and the limit is %d",
				len(subject), emailSubjectMax), "code": "too_large",
			"hint": fmt.Sprintf("shorten the subject to %d characters and put the rest in text", emailSubjectMax)})
		return
	case strings.TrimSpace(body.Text) == "":
		// Required even with HTML: the plain-text half is where the "an
		// application sent this" line is unmissable, and an HTML-only message
		// has no fallback for a client that will not render it.
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "'text' is required, even when 'html' is given",
			"hint":  "pass a plain-text body as text — it is required even beside html"})
		return
	case len(body.Text) > kind.TextLimit:
		writeNotifyTooLarge(w, fmt.Sprintf("the text body is %d bytes and the limit is %d",
			len(body.Text), kind.TextLimit),
			fmt.Sprintf("shorten the text to %d bytes — or put it in storage and send a link", kind.TextLimit))
		return
	case len(body.HTML) > emailHTMLLimit:
		writeNotifyTooLarge(w, fmt.Sprintf("the html body is %d bytes and the limit is %d",
			len(body.HTML), emailHTMLLimit),
			fmt.Sprintf("shorten the html to %d bytes — or put it in storage and send a link", emailHTMLLimit))
		return
	}

	// Attachments are gated exactly as sending is (migration 44). The capability
	// still splits here because it is what the ledger records as the send's
	// `action`, and "which of these carried a file" is a question the Integration
	// log has to be able to answer.
	action := kind.TextAction()
	if len(files) > 0 {
		action = kind.FileAction()
	}

	digest, size := appint.Digest(subject, body.Text, body.HTML)
	payload := appint.Payload{
		Subject: subject,
		To:      to.To,
		CC:      to.CC,
		BCC:     to.BCC,
		Text:    body.Text,
		HTML:    body.HTML,
		Files:   files,
	}
	s.acceptSend(w, r, token, kind.Capability(action.Name), payload, sendRecord{
		Recipients:  to.All,
		Subject:     subject,
		BodySHA256:  digest,
		BodyBytes:   size,
		Body:        body.Text,
		Attachments: describeFiles(files),
	})
}

// refuseDestinationFields is ADR §25 enforced on the wire, once, for every kind.
func refuseDestinationFields(w http.ResponseWriter, kindTitle string, raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	for field, what := range destinationFields {
		v, ok := probe[field]
		if !ok || string(v) == "null" || string(v) == `""` {
			continue
		}
		writeDestinationRefusal(w, kindTitle, field, what)
		return true
	}
	return false
}

// The sentence names the integration the send went to: it said "its Slack
// channel" to an email or a Teams send (BB-060, fifth 2026-11-03 black-box
// list), and "a send to" because "a Email send" read wrong (BB-073).
func writeDestinationRefusal(w http.ResponseWriter, kindTitle, field, what string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error": "a send to " + kindTitle + " does not take " + strconv.Quote(field) + ": an app does not choose " + what + " — its owners choose it on the app's " +
			"Integrations tab, from what the connection an administrator made can reach. " +
			"Remove the " + strconv.Quote(field) + " field.",
		"code": "destination_not_allowed",
		"hint": "remove the " + strconv.Quote(field) + " field; the app's owners choose the destination on its Integrations tab"})
}

// sendRecord is what goes in the ledger, as opposed to what goes on the wire.
type sendRecord struct {
	Recipients  []string
	Subject     string
	BodySHA256  string
	BodyBytes   int
	Body        string
	Attachments []database.SendAttachment
}

// acceptSend is steps 2 to 5, shared by every route above. One function so that
// a new integration cannot accidentally skip the gate or the record.
func (s *Server) acceptSend(w http.ResponseWriter, r *http.Request,
	token *database.ResolvedAppToken, capability string,
	payload appint.Payload, record sendRecord) {

	appID := token.Token.AppID

	decision, err := appint.Check(s.db, appID, token.AppStatus, capability)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if !decision.Allowed {
		writeJSON(w, decision.Status, decisionBody(decision))
		return
	}

	// An idempotent replay: the same app, the same key, inside the window.
	// Answered with the original record and nothing is sent.
	key := strings.TrimSpace(r.Header.Get(idempotencyLabel))
	if key != "" {
		existing, err := s.db.FindSendByIdempotencyKey(appID, key, appint.IdempotencyWindow)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if existing != nil {
			writeJSON(w, http.StatusOK, replayedSend(existing))
			return
		}
	}

	send, err := s.queueSend(sendRequest{
		AppID:          appID,
		AppStatus:      token.AppStatus,
		AppName:        token.AppName,
		AppSlug:        token.AppSlug,
		DeploymentID:   token.Token.DeploymentID,
		Capability:     capability,
		Payload:        payload,
		Record:         record,
		IdempotencyKey: key,
	})
	switch {
	case errors.Is(err, appint.ErrQueueFull):
		msg := "the platform's send queue is full — try again in a few seconds"
		if err != appint.ErrQueueFull {
			// This app's own share of the queue is used up: the backlog is
			// its own, and saying so is what tells it to slow down.
			msg = err.Error() + " — wait for them to go out, then try again"
		}
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{
			"error": msg,
			"code":  "queue_full",
			"hint":  "wait a few seconds, honoring Retry-After, and send again"})
		return
	case errors.Is(err, errDuplicateSend):
		// The race the check above does not cover: two identical calls in
		// flight at once. The second is still a duplicate, and saying so beats
		// a 500.
		if existing, e := s.db.FindSendByIdempotencyKey(appID, key, appint.IdempotencyWindow); e == nil && existing != nil {
			writeJSON(w, http.StatusOK, replayedSend(existing))
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// duplicate is always present, false on a first send, as a job start's
	// deduplicated is — so an R app reading the platform's body as its result
	// can compare it without first asking whether the key exists (2026-11-03
	// evening drills).
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"send_id": send.ID, "status": database.SendQueued, "duplicate": false})
}

// replayedSend is the answer to an idempotent replay: the original send, its
// status, and — once it has one — its failure. Without the error a replay of a
// failed send read as a send with nothing wrong (Go's Send.Err was ""), while
// notify.status on the same id carried the relay's refusal.
func replayedSend(existing *database.IntegrationSend) map[string]interface{} {
	out := map[string]interface{}{"send_id": existing.ID, "status": existing.Status, "duplicate": true}
	if existing.Error != "" {
		out["error"] = existing.Error
	}
	return out
}

// sendRequest is everything queueSend needs. A struct rather than nine
// parameters because the two call sites — an app's own token and an
// administrator's test button — differ in only two of them, and a positional
// list is where that difference goes wrong silently.
type sendRequest struct {
	AppID          string
	AppStatus      string
	AppName        string
	AppSlug        string
	DeploymentID   string
	Capability     string
	Payload        appint.Payload
	Record         sendRecord
	IdempotencyKey string
}

// errDuplicateSend is a unique-violation on (app_id, idempotency_key).
var errDuplicateSend = errors.New("a send with this idempotency key already exists")

// queueSend writes the ledger row and hands the job to the queue.
//
// The row is written *before* anything is attempted, which is what lets the 202
// the caller receives be a promise the record can keep. It does not run the
// gates: both callers have already, and re-running them here would put the same
// decision in two places.
func (s *Server) queueSend(req sendRequest) (*database.IntegrationSend, error) {
	if s.appSender == nil {
		return nil, errors.New("app integrations are not available on this install")
	}

	kind := appint.IntegrationOf(req.Capability)
	// Resolved now, so the ledger can name where a message was going even if it
	// never got there — and so the worker delivers where the record says rather
	// than wherever a second lookup would have landed.
	attachment, err := s.db.ResolveAttachment(req.AppID, kind)
	if err != nil {
		return nil, err
	}
	destination := attachment.Destination.Label
	if destination == "" {
		destination = attachment.Destination.ID
	}

	settings, err := s.db.GetIntegrationSettings()
	if err != nil {
		return nil, err
	}
	body := ""
	if settings.RetainBodies {
		body = req.Record.Body
	}

	send := &database.IntegrationSend{
		ID:             uuid.NewString(),
		AppID:          req.AppID,
		Kind:           kind,
		Action:         appint.ActionOf(req.Capability),
		Status:         database.SendQueued,
		Destination:    destination,
		Recipients:     req.Record.Recipients,
		Subject:        req.Record.Subject,
		BodyBytes:      req.Record.BodyBytes,
		BodySHA256:     req.Record.BodySHA256,
		Body:           body,
		Attachments:    req.Record.Attachments,
		IdempotencyKey: req.IdempotencyKey,
		DeploymentID:   req.DeploymentID,
	}
	if err := s.db.InsertSend(send); err != nil {
		if isDuplicateKey(err) {
			return nil, errDuplicateSend
		}
		return nil, err
	}

	job := appint.Job{
		Send:         send,
		Payload:      req.Payload,
		Capability:   req.Capability,
		AppStatus:    req.AppStatus,
		AppName:      req.AppName,
		AppSlug:      req.AppSlug,
		BaseURL:      s.appLinkBase(),
		ConnectionID: attachment.ConnectionID,
		Destination:  attachment.Destination,
	}
	if err := s.appSender.Enqueue(job); err != nil {
		// Failed in the ledger, and no longer the answer to this
		// Idempotency-Key: the 429 says "try again", every SDK does, and the
		// resend used to match this very row and come back 200 as a
		// duplicate of a failed send — accepted, and never sent (code review
		// 2026-11-11, I-2).
		_ = s.db.CompleteSend(send.ID, database.SendFailed, "the platform's send queue was full")
		_ = s.db.ReleaseSendIdempotencyKey(send.ID)
		return nil, err
	}
	return send, nil
}

// ------------------------------------------------------------------ helpers

type recipientSet struct {
	To, CC, BCC, All []string
}

// normalizeRecipients parses every address and re-renders it from the parse,
// which is what neutralizes anything hiding in a display name. A bad address is
// named rather than dropped: silently discarding one recipient is how a report
// misses the person it was for.
func normalizeRecipients(to, cc, bcc []string) (recipientSet, error) {
	var out recipientSet
	clean := func(in []string) ([]string, error) {
		result := make([]string, 0, len(in))
		for _, addr := range in {
			if strings.TrimSpace(addr) == "" {
				continue
			}
			parsed, err := mailer.ValidateRecipient(addr)
			if err != nil {
				return nil, err
			}
			result = append(result, parsed)
			out.All = append(out.All, strings.ToLower(parsed))
		}
		return result, nil
	}
	var err error
	if out.To, err = clean(to); err != nil {
		return out, err
	}
	if out.CC, err = clean(cc); err != nil {
		return out, err
	}
	if out.BCC, err = clean(bcc); err != nil {
		return out, err
	}
	return out, nil
}

// parseAppMultipart parses a multipart send with the whole body capped first.
//
// ParseMultipartForm keeps multipartMemory in memory and spills the rest of
// every file part to TMPDIR before anything here looks at a size, so without
// the cap one app token could stream an unbounded body onto the disk that
// every concurrent build writes to — a small tmpfs on this box and in the
// one-box bundle. The ceiling is the most a legitimate send can carry: the
// kind's file allowance, its text and HTML, and a megabyte for the framing and
// the other fields.
func parseAppMultipart(w http.ResponseWriter, r *http.Request, kind *appint.Kind) error {
	limit := int64(kind.FileLimit) + int64(kind.TextLimit) + emailHTMLLimit + 1<<20
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	return r.ParseMultipartForm(multipartMemory)
}

// tooLarge answers 413 when err is the body cap, and reports whether it did.
func tooLarge(w http.ResponseWriter, err error) bool {
	var mbe *http.MaxBytesError
	if !errors.As(err, &mbe) {
		return false
	}
	writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
		"error": fmt.Sprintf("the request is larger than the %d bytes this send can carry", mbe.Limit),
		"code":  "too_large",
		"hint": fmt.Sprintf("send under %d bytes in all: shorten the text, or attach fewer or smaller files — "+
			"or put the file in storage and send a link to it", mbe.Limit)})
	return true
}

// readNotifyBody reads a JSON send body under a cap, and answers 413 too_large
// when the body is over it. It was a LimitReader, which cut a long body short
// and handed the parser half a document, so valid JSON over the cap came back
// 400 "invalid JSON body" (BB-028, fourth 2026-11-03 black-box list).
func readNotifyBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		if tooLarge(w, err) {
			return nil, false
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "could not read the body",
			"hint": "send the request again; the body did not arrive whole"})
		return nil, false
	}
	return raw, true
}

// writeNotifyTooLarge is a message over a size limit: 413 too_large, the
// published code, with the next step. It was 400 too_long, which no contract
// named (BB-028, fourth 2026-11-03 black-box list).
// The hint names the limit, as the subject's does (BB-052, fifth 2026-11-03
// black-box list).
func writeNotifyTooLarge(w http.ResponseWriter, msg, hint string) {
	writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
		"error": msg, "code": "too_large", "hint": hint})
}

func writeNotifyBadJSON(w http.ResponseWriter) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body",
		"hint": "send one JSON object with Content-Type: application/json"})
}

func writeNoAppToken(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no app token",
		"hint": "call this from inside a deployed app or one of its jobs, where the SDK finds its credential"})
}

// textActionNames lists a kind's text actions, for a hint.
func textActionNames(kind *appint.Kind) string {
	var names []string
	for _, a := range kind.Actions {
		if a.Carries == appint.CarriesText {
			names = append(names, a.Name)
		}
	}
	return strings.Join(names, ", ")
}

// readFileParts reads up to max attachments, refusing at totalLimit.
func readFileParts(r *http.Request, field string, max, totalLimit int) ([]appint.File, error) {
	if r.MultipartForm == nil {
		return nil, nil
	}
	headers := r.MultipartForm.File[field]
	if len(headers) > max {
		return nil, fmt.Errorf("%d files, and the limit for one message is %d", len(headers), max)
	}
	out := make([]appint.File, 0, len(headers))
	total := 0
	for _, header := range headers {
		f, err := header.Open()
		if err != nil {
			return nil, fmt.Errorf("could not read %q", header.Filename)
		}
		content, err := io.ReadAll(io.LimitReader(f, int64(totalLimit)+1))
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("could not read %q", header.Filename)
		}
		total += len(content)
		if total > totalLimit {
			return nil, fmt.Errorf("the attachments total more than %d bytes", totalLimit)
		}
		out = append(out, appint.File{
			// Reduced to a basename: a filename carrying somebody's directory
			// layout into a channel or an inbox is a small disclosure nobody
			// chose. Never trusted from the caller.
			Name: safeFilename(header.Filename),
			// Sniffed rather than taken from the caller's Content-Type.
			Type:    http.DetectContentType(content),
			Content: content,
		})
	}
	return out, nil
}

func safeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' {
			return -1
		}
		return r
	}, strings.TrimSpace(name))
	if name == "" || name == "." || name == ".." {
		return "attachment"
	}
	return name
}

func describeFiles(files []appint.File) []database.SendAttachment {
	out := make([]database.SendAttachment, 0, len(files))
	for _, f := range files {
		out = append(out, database.SendAttachment{
			Name:   f.Name,
			Bytes:  len(f.Content),
			Type:   f.Type,
			SHA256: appint.DigestBytes(f.Content),
		})
	}
	return out
}

func isDuplicateKey(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate key")
}

// decisionBody is an integration gate's refusal as the envelope: the sentence,
// the code and, where the gate names one, the next step.
func decisionBody(d appint.Decision) map[string]string {
	body := map[string]string{"error": d.Reason, "code": d.Code}
	if d.Hint != "" {
		body["hint"] = d.Hint
	}
	return body
}

// notifyFilesHint is too_large's next step for attachments, naming the limit
// (BB-052, fifth 2026-11-03 black-box list).
func notifyFilesHint(limit int) string {
	return fmt.Sprintf("keep the files under %d bytes in all: attach fewer or smaller files — or put the file "+
		"in storage and send a link to it", limit)
}
