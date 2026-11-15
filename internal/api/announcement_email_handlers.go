package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"toft/internal/announce"
	"toft/internal/auth"
	"toft/internal/database"
)

// The announcement an administrator sends — the compose box, the freeze, and
// the record it produces.
//
// **Ten routes, and the handlers' whole job is to decide and then hand over.**
// Everything that can go wrong once a send has started belongs to
// internal/announce, which owns the goroutine; what is here is authorization,
// the caps, the link, and the two refusals that have to happen *before* the
// freeze — no relay, and an audience that resolves to nobody. A frozen row that
// mailed nobody is a record of nothing.
//
// Every route is admin-only and answers 404 to everybody else, matching
// /system/settings and the audit feed: the existence of this install's
// announcement history is not owed to a viewer. Every route is also refused to
// a personal API key, by name, in keyroutes.go — deliberately the opposite of
// the banner beside it, which a key may publish. The banner is one idempotent
// string that DELETE takes back; a send reaches every person on the install,
// cannot be taken back, and leaves a permanent row, and a leaked `toftu_` that
// could mail the whole directory in this install's voice is a phishing platform
// rather than a bounded incident.

// Announcer is the send runner. An interface so a Server built in a test can be
// handed one that does not need a relay — and so that the API layer holds no
// sending code of its own, which is the seam that keeps every interesting
// failure mode inside internal/announce where it can be tested directly.
type Announcer interface {
	// Available reports whether this install can send at all.
	Available() bool
	// Start runs one frozen announcement in the background.
	Start(id string)
	// SendTest delivers one message to one address, synchronously.
	SendTest(ctx context.Context, a *database.AnnouncementEmail, to string) error
}

type announcementEmailRequest struct {
	Subject  *string `json:"subject"`
	Body     *string `json:"body"`
	LinkURL  *string `json:"link_url"`
	LinkText *string `json:"link_text"`
	Audience *string `json:"audience"`
}

// announcementEmailResponse is one announcement plus what the surface needs to
// decide what to offer. `can_send` is the relay, not a permission: the caller is
// already an administrator by the time they can read this.
type announcementEmailResponse struct {
	database.AnnouncementEmail
	Recipients []database.AnnouncementRecipient `json:"recipients,omitempty"`
	CanSend    bool                             `json:"can_send"`
}

// GET /api/v1/admin/announcement-emails — the install's whole history.
func (s *Server) handleListAnnouncementEmails(w http.ResponseWriter, r *http.Request) {
	if s.adminPrincipal(w, r) == nil {
		return
	}
	list, err := s.db.ListAnnouncementEmails()
	if err != nil {
		log.Printf("announcement emails: listing: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"announcements": list,
		"can_send":      s.announcerOrNil().Available(),
	})
}

// GET /api/v1/admin/announcement-emails/estimate?audience=all — about how many
// people this audience resolves to today.
//
// Its own route rather than a field on the list, because the composer asks it
// again every time the switch moves and a draft composed on Monday and sent on
// Thursday is sent to Thursday's directory. Nothing is frozen by counting.
func (s *Server) handleEstimateAnnouncementAudience(w http.ResponseWriter, r *http.Request) {
	if s.adminPrincipal(w, r) == nil {
		return
	}
	audience := strings.TrimSpace(r.URL.Query().Get("audience"))
	if !database.ValidAnnouncementAudience(audience) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "audience must be all or publishers", "code": "bad_audience",
			"hint": "send ?audience=all or ?audience=publishers"})
		return
	}
	est, err := s.db.EstimateAnnouncementAudience(audience)
	if err != nil {
		log.Printf("announcement emails: estimating %s: %v", audience, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, est)
}

// GET /api/v1/admin/announcement-emails/{id} — one announcement and every
// recipient row it has.
//
// One read rather than two, because the only reason to open this page is to
// find out who did not get it, and a second request for the answer to the
// question the page exists to ask is a spinner nobody needs.
func (s *Server) handleGetAnnouncementEmail(w http.ResponseWriter, r *http.Request) {
	if s.adminPrincipal(w, r) == nil {
		return
	}
	a := s.loadAnnouncement(w, r)
	if a == nil {
		return
	}
	recipients, err := s.db.ListAnnouncementRecipients(a.ID)
	if err != nil {
		log.Printf("announcement emails: recipients of %s: %v", a.ID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, announcementEmailResponse{
		AnnouncementEmail: *a,
		Recipients:        recipients,
		CanSend:           s.announcerOrNil().Available(),
	})
}

// POST /api/v1/admin/announcement-emails — a new draft.
func (s *Server) handleCreateAnnouncementEmail(w http.ResponseWriter, r *http.Request) {
	p := s.adminPrincipal(w, r)
	if p == nil {
		return
	}
	var req announcementEmailRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body", "hint": badBodyHint})
		return
	}
	a := &database.AnnouncementEmail{Audience: database.AnnouncementAudienceAll, CreatedBy: p.ID}
	applyAnnouncementRequest(a, req)
	if !s.validAnnouncement(w, a) {
		return
	}
	if err := s.db.CreateAnnouncementEmail(a); err != nil {
		log.Printf("announcement emails: creating: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	s.auditAnnouncement(r, p, "announcement_email.create", a, jsonValStr(announcementAudit(a)), "")
	writeJSON(w, http.StatusCreated, announcementEmailResponse{
		AnnouncementEmail: *a, CanSend: s.announcerOrNil().Available()})
}

// PATCH /api/v1/admin/announcement-emails/{id} — edit a draft.
//
// A frozen row refuses with 409 and its current status. The refusal is in the
// SQL rather than in a read followed by a write: content stops being editable
// the moment the first recipient could have received it, and a body edited at
// recipient 300 would mean two different messages went out under one record.
func (s *Server) handleUpdateAnnouncementEmail(w http.ResponseWriter, r *http.Request) {
	p := s.adminPrincipal(w, r)
	if p == nil {
		return
	}
	a := s.loadAnnouncement(w, r)
	if a == nil {
		return
	}
	before := jsonValStr(announcementAudit(a))

	var req announcementEmailRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body", "hint": badBodyHint})
		return
	}
	applyAnnouncementRequest(a, req)
	if !s.validAnnouncement(w, a) {
		return
	}
	ok, err := s.db.UpdateAnnouncementDraft(a)
	if err != nil {
		log.Printf("announcement emails: updating %s: %v", a.ID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if !ok {
		s.refuseFrozen(w, a.ID, "this announcement has already been sent and cannot be edited")
		return
	}
	s.auditAnnouncement(r, p, "announcement_email.update", a, jsonValStr(announcementAudit(a)), before)
	writeJSON(w, http.StatusOK, announcementEmailResponse{
		AnnouncementEmail: *a, CanSend: s.announcerOrNil().Available()})
}

// DELETE /api/v1/admin/announcement-emails/{id} — a draft, or a record.
//
// Two of the three statuses. A draft has reached nobody; a `sent` row is the
// install's own history, and an administrator is allowed to be rid of their own
// history — a mistaken subject line, a test send to the whole directory, a
// record naming people who have since left. The recipient rows go with it by
// cascade.
//
// What the record does not get to do is vanish silently, which is the whole of
// what is kept from the old rule: the audit row this writes carries the
// subject, the audience, when it went, who sent it and all five counts, so
// *"on the 3rd of March this install told 214 people the queue was down"*
// survives the deletion of the thing that said so. That row is on the audit
// log's retention rather than on the announcement's, and no route here can
// remove it.
//
// A `sending` row is refused, and that refusal is not a policy: the runner is
// still writing recipient rows and a final count into this id, and deleting
// underneath it is a goroutine writing to a row that is gone. It is deletable
// the moment the run completes.
func (s *Server) handleDeleteAnnouncementEmail(w http.ResponseWriter, r *http.Request) {
	p := s.adminPrincipal(w, r)
	if p == nil {
		return
	}
	a := s.loadAnnouncement(w, r)
	if a == nil {
		return
	}
	// Audited before the delete rather than after, because the row is what the
	// audit payload is built from and a cascade does not hand it back.
	before := jsonValStr(announcementDeleteAudit(a))

	ok, err := s.db.DeleteAnnouncementEmail(a.ID)
	if err != nil {
		log.Printf("announcement emails: deleting %s: %v", a.ID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if !ok {
		s.refuseSending(w, a.ID,
			"this announcement is going out right now. It can be deleted once the run has finished")
		return
	}
	s.auditAnnouncement(r, p, "announcement_email.delete", a, "", before)
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/v1/admin/announcement-emails/{id}/clone — the content, not the
// record.
func (s *Server) handleCloneAnnouncementEmail(w http.ResponseWriter, r *http.Request) {
	p := s.adminPrincipal(w, r)
	if p == nil {
		return
	}
	src := s.loadAnnouncement(w, r)
	if src == nil {
		return
	}
	clone := &database.AnnouncementEmail{
		Subject:  src.Subject,
		Body:     src.Body,
		LinkURL:  src.LinkURL,
		LinkText: src.LinkText,
		Audience: src.Audience,
		// Everything about the original send is left behind. cloned_from
		// carries no foreign key: the source may be deleted, and "cloned from
		// something that is gone" is still a true thing to have recorded.
		ClonedFrom: src.ID,
		CreatedBy:  p.ID,
	}
	if err := s.db.CreateAnnouncementEmail(clone); err != nil {
		log.Printf("announcement emails: cloning %s: %v", src.ID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	s.auditAnnouncement(r, p, "announcement_email.create", clone,
		jsonValStr(announcementAudit(clone)), "")
	writeJSON(w, http.StatusCreated, announcementEmailResponse{
		AnnouncementEmail: *clone, CanSend: s.announcerOrNil().Available()})
}

// POST /api/v1/admin/announcement-emails/{id}/test — the real template, the
// real content, to the caller's own address.
//
// It freezes nothing and records no recipient row. There is no un-send: a
// message that reaches every person on an install with a typo in it is not a
// bug that can be fixed, and a preview pane does not test the relay, the From
// address, the link, or what the body looks like in Outlook.
func (s *Server) handleTestAnnouncementEmail(w http.ResponseWriter, r *http.Request) {
	p := s.adminPrincipal(w, r)
	if p == nil {
		return
	}
	a := s.loadAnnouncement(w, r)
	if a == nil {
		return
	}
	if !s.announcerOrNil().Available() {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "no mail relay is configured on this install — set one up in Admin → Email",
			"code":  "relay_not_configured",
			"hint":  "set the relay up with PUT /api/v1/admin/email-settings, then try again"})
		return
	}
	to := s.callerEmail(p)
	if to == "" {
		// The first time most administrators meet the skip rule, and the right
		// place to meet it: an account with no address on file is exactly what
		// `no_address` skips, and the composer is the only surface anybody
		// would ever learn that from.
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "your account has no email address on file, so there is nowhere to send the test",
			"code":  "no_address",
			"hint":  "sign in with an account that has an email address and send the test from there"})
		return
	}
	if err := s.announcerOrNil().SendTest(r.Context(), a, to); err != nil {
		// The relay's own words. An administrator pressing a test button is
		// waiting for exactly that, and "sending failed" is a sentence they can
		// do nothing with.
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": err.Error(), "code": "send_failed"})
		return
	}
	s.auditAnnouncement(r, p, "announcement_email.test", a,
		jsonValStr(map[string]string{"to": to}), "")
	writeJSON(w, http.StatusOK, map[string]string{"sent_to": to})
}

// POST /api/v1/admin/announcement-emails/{id}/send — the freeze, then the run.
//
// The order here is the whole of this handler. Both refusals happen *before*
// the freeze, because a frozen row that mailed nobody is a record of nothing:
// no relay is 409, and an audience that resolves to zero people is 409. Then
// one guarded UPDATE decides who sends — two administrators on the Send button
// of the same draft is the ordinary case on a shared install, and zero rows
// affected is the second of them.
func (s *Server) handleSendAnnouncementEmail(w http.ResponseWriter, r *http.Request) {
	p := s.adminPrincipal(w, r)
	if p == nil {
		return
	}
	a := s.loadAnnouncement(w, r)
	if a == nil {
		return
	}
	if !s.announcerOrNil().Available() {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "no mail relay is configured on this install — set one up in Admin → Email",
			"code":  "relay_not_configured",
			"hint":  "set the relay up with PUT /api/v1/admin/email-settings, then try again"})
		return
	}
	if a.Status != database.AnnouncementDraft {
		s.refuseFrozen(w, a.ID, "this announcement has already been sent")
		return
	}
	// Re-validated here as well as at the save, because this is the last
	// decision before a URL wearing this install's From address is mailed to
	// everybody.
	if err := announce.ValidateLink(a.LinkURL); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": err.Error(), "field": "link_url", "code": "bad_link",
			"hint": `PATCH /api/v1/admin/announcement-emails/` + a.ID + ` with a link_url starting https://, or an empty one, then send again`})
		return
	}
	est, err := s.db.EstimateAnnouncementAudience(a.Audience)
	if err != nil {
		log.Printf("announcement emails: estimating %s: %v", a.ID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if est.Total == 0 {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "nobody is in that audience, so there is nothing to send",
			"code":  "empty_audience",
			"hint":  "choose the other audience with PATCH /api/v1/admin/announcement-emails/" + a.ID + ", or send once someone is in this one"})
		return
	}

	frozen, err := s.db.BeginAnnouncementSend(a.ID, p.ID)
	if err != nil {
		log.Printf("announcement emails: freezing %s: %v", a.ID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if !frozen {
		s.refuseFrozen(w, a.ID, "somebody else has already sent this announcement")
		return
	}

	// The audit row carries the audience, the resolved estimate and the
	// subject. Not the body: it is on the row itself, and it is not what an
	// audit reader is scanning for.
	s.auditAnnouncement(r, p, "announcement_email.send", a, jsonValStr(map[string]any{
		"audience": a.Audience,
		"subject":  a.Subject,
		"resolved": est.Total,
	}), "")

	s.announcerOrNil().Start(a.ID)

	sent, err := s.db.GetAnnouncementEmail(a.ID)
	if err != nil || sent == nil {
		writeJSON(w, http.StatusAccepted, announcementEmailResponse{
			AnnouncementEmail: *a, CanSend: true})
		return
	}
	writeJSON(w, http.StatusAccepted, announcementEmailResponse{
		AnnouncementEmail: *sent, CanSend: true})
}

// POST /api/v1/admin/announcement-emails/{id}/resend — the same frozen
// message, to the people its run did not reach.
//
// A new row rather than a second run on this one: `sent` is terminal, and the
// original's record — *sent to 214, 12 failed* — stays a true statement about
// the day it went out. The resend is created already sending (there is nothing
// to edit; the content is the original's), and it resolves from the original's
// failed and abandoned rows, through each account as it is now.
//
// Every refusal comes before the row exists, for the reason the send's do: no
// relay, an original that is not `sent`, one already resent, and nobody left to
// resend to — which is also the answer once the per-person rows were pruned.
func (s *Server) handleResendAnnouncementEmail(w http.ResponseWriter, r *http.Request) {
	p := s.adminPrincipal(w, r)
	if p == nil {
		return
	}
	a := s.loadAnnouncement(w, r)
	if a == nil {
		return
	}
	if !s.announcerOrNil().Available() {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "no mail relay is configured on this install — set one up in Admin → Email",
			"code":  "relay_not_configured",
			"hint":  "set the relay up with PUT /api/v1/admin/email-settings, then try again"})
		return
	}
	switch a.Status {
	case database.AnnouncementDraft:
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "this announcement has not been sent yet, so there is nobody to resend it to",
			"code":  "not_sent", "status": a.Status,
			"hint": "POST /api/v1/admin/announcement-emails/" + a.ID + "/send sends it"})
		return
	case database.AnnouncementSending:
		s.refuseSending(w, a.ID,
			"this announcement is going out right now. It can be resent once the run has finished")
		return
	}
	if a.ResentAs != "" {
		s.refuseResent(w, a.ID, a.ResentAs)
		return
	}
	n, err := s.db.CountAnnouncementResendable(a.ID)
	if err != nil {
		log.Printf("announcement emails: counting the resend of %s: %v", a.ID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if n == 0 {
		msg := "everybody this announcement could reach received it, so there is nobody to resend it to"
		if a.Failed+a.Abandoned > 0 {
			msg = "the people this announcement did not reach are no longer on record — the per-person list was pruned, or their accounts are gone"
		}
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": msg, "code": "nothing_to_resend",
			"hint": "POST /api/v1/admin/announcement-emails/" + a.ID + "/clone makes a draft that can be sent to a whole audience"})
		return
	}

	resend, err := s.db.CreateAnnouncementResend(a.ID, p.ID)
	if errors.Is(err, database.ErrAlreadyResent) {
		cur, _ := s.db.GetAnnouncementEmail(a.ID)
		resentAs := ""
		if cur != nil {
			resentAs = cur.ResentAs
		}
		// A resend that never resolved anybody — the process died, or the
		// resolve failed, between its insert and its recipients — was
		// finished as sent with no rows, and then consumed the original's one
		// resend for good (code review 2026-11-11, P2-1). Such a row is
		// removed and the resend made again.
		if prior, _ := s.db.GetAnnouncementEmail(resentAs); prior != nil && prior.Status == database.AnnouncementSent {
			if people, perr := s.db.ListAnnouncementRecipients(prior.ID); perr == nil && len(people) == 0 {
				if _, derr := s.db.DeleteAnnouncementEmail(prior.ID); derr == nil {
					resend, err = s.db.CreateAnnouncementResend(a.ID, p.ID)
				}
			}
		}
		if errors.Is(err, database.ErrAlreadyResent) {
			s.refuseResent(w, a.ID, resentAs)
			return
		}
	}
	if err != nil {
		log.Printf("announcement emails: resending %s: %v", a.ID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if resend == nil {
		// The original moved off `sent` between the read and the insert —
		// deleted, since nothing else moves a sent row.
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/admin/announcement-emails lists them by id"})
		return
	}

	s.auditAnnouncement(r, p, "announcement_email.resend", resend, jsonValStr(map[string]any{
		"resend_of": a.ID,
		"subject":   a.Subject,
		"resolved":  n,
	}), "")

	s.announcerOrNil().Start(resend.ID)
	writeJSON(w, http.StatusAccepted, announcementEmailResponse{
		AnnouncementEmail: *resend, CanSend: true})
}

// refuseResent is the resend's 409 when one exists already, and it names it:
// what the resend did not reach in turn is resent from there.
func (s *Server) refuseResent(w http.ResponseWriter, id, resentAs string) {
	body := map[string]string{
		"error": "this announcement has already been resent", "code": "already_resent",
		"resent_as": resentAs,
		"hint":      "GET /api/v1/admin/announcement-emails/" + resentAs + " is the resend; resend from it to reach whoever it missed",
	}
	if resentAs == "" {
		body["hint"] = "GET /api/v1/admin/announcement-emails/" + id + " names the resend in resent_as"
	}
	writeJSON(w, http.StatusConflict, body)
}

// loadAnnouncement resolves the id in the URL, 404ing when there is no such
// row. The IDOR rule in the other direction is not in play — every route here
// is already admin-only — but the shape is the same one every id-taking handler
// uses.
func (s *Server) loadAnnouncement(w http.ResponseWriter, r *http.Request) *database.AnnouncementEmail {
	id := chi.URLParam(r, "id")
	a, err := s.db.GetAnnouncementEmail(id)
	if err != nil {
		log.Printf("announcement emails: loading %s: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return nil
	}
	if a == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found", "hint": "GET /api/v1/admin/announcement-emails lists them by id"})
		return nil
	}
	return a
}

// refuseFrozen is the 409 a frozen row answers with when it is too late to
// change what is going out.
func (s *Server) refuseFrozen(w http.ResponseWriter, id, message string) {
	s.refuseAnnouncement(w, id, "already_sent", message)
}

// refuseSending is the delete's 409, and it is a different sentence: the row is
// not too late to change, it is busy. Its own code, because the surface offers
// the delete again the moment the run finishes.
func (s *Server) refuseSending(w http.ResponseWriter, id, message string) {
	s.refuseAnnouncement(w, id, "send_in_flight", message)
}

// refuseAnnouncement re-reads the status rather than trusting the one the
// caller loaded, because the whole reason either path was reached is that
// something moved underneath it.
func (s *Server) refuseAnnouncement(w http.ResponseWriter, id, code, message string) {
	status := database.AnnouncementSending
	if a, err := s.db.GetAnnouncementEmail(id); err == nil && a != nil {
		status = a.Status
	}
	writeJSON(w, http.StatusConflict, map[string]string{
		"error": message, "code": code, "status": status,
		"hint": refusalHintFor(code, id)})
}

// validAnnouncement holds the caps and the link rule. An overlong field refuses
// the save rather than being truncated: a message going out carrying words its
// author never wrote is worse than a refusal.
func (s *Server) validAnnouncement(w http.ResponseWriter, a *database.AnnouncementEmail) bool {
	a.Subject = strings.TrimSpace(a.Subject)
	a.Body = strings.TrimSpace(a.Body)
	a.LinkURL = strings.TrimSpace(a.LinkURL)
	a.LinkText = strings.TrimSpace(a.LinkText)

	switch {
	case a.Subject == "":
		return badAnnouncement(w, "subject", "an announcement needs a subject")
	case len([]rune(a.Subject)) > announce.MaxSubject:
		return badAnnouncement(w, "subject", "the subject is longer than 200 characters")
	case a.Body == "":
		return badAnnouncement(w, "body", "an announcement needs something to say")
	case len([]rune(a.Body)) > announce.MaxBody:
		return badAnnouncement(w, "body", "the message is longer than 4,000 characters")
	case len([]rune(a.LinkText)) > announce.MaxLinkText:
		return badAnnouncement(w, "link_text", "the link text is longer than 60 characters")
	case !database.ValidAnnouncementAudience(a.Audience):
		return badAnnouncement(w, "audience", "audience must be all or publishers")
	}
	if err := announce.ValidateLink(a.LinkURL); err != nil {
		return badAnnouncement(w, "link_url", err.Error())
	}
	// A link with no text renders under a default rather than as a bare URL,
	// and a link text with no link is a button pointing nowhere.
	if a.LinkURL == "" && a.LinkText != "" {
		return badAnnouncement(w, "link_url", "there is link text but no link")
	}
	return true
}

// refusalHintFor is the next step after a 409 on a draft that moved: a send in
// flight is waited out, and a sent one is copied rather than edited.
func refusalHintFor(code, id string) string {
	if code == "send_in_flight" {
		return "wait for the send to finish — GET /api/v1/admin/announcement-emails/" + id + " shows its status — then try again"
	}
	return "POST /api/v1/admin/announcement-emails/" + id + "/clone makes an editable copy"
}

// announcementFieldHint is the next step for each field validAnnouncement
// refuses.
func announcementFieldHint(field string) string {
	switch field {
	case "subject":
		return "send a subject of 200 characters or fewer"
	case "body":
		return "send a body of 4,000 characters or fewer"
	case "link_text":
		return "shorten link_text to 60 characters or fewer, or leave it empty"
	case "audience":
		return `send "audience": "all" or "publishers"`
	case "link_url":
		return "send a link_url starting https://, or leave link_text empty too"
	}
	return "correct " + field + " and save again"
}

func badAnnouncement(w http.ResponseWriter, field, message string) bool {
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error": message, "field": field, "code": "invalid",
		"hint": announcementFieldHint(field)})
	return false
}

// applyAnnouncementRequest is the PATCH shape: a field the caller did not send
// is a field they did not change.
func applyAnnouncementRequest(a *database.AnnouncementEmail, req announcementEmailRequest) {
	if req.Subject != nil {
		a.Subject = *req.Subject
	}
	if req.Body != nil {
		a.Body = *req.Body
	}
	if req.LinkURL != nil {
		a.LinkURL = *req.LinkURL
	}
	if req.LinkText != nil {
		a.LinkText = *req.LinkText
	}
	if req.Audience != nil {
		a.Audience = *req.Audience
	}
}

// announcementAudit is what an audit row carries about an announcement: what it
// was addressed to and what it said, never the whole body. The body is on the
// row itself, and an audit reader scanning a year of rows is looking for the
// subject.
func announcementAudit(a *database.AnnouncementEmail) map[string]any {
	return map[string]any{
		"subject":  a.Subject,
		"audience": a.Audience,
		"link_url": a.LinkURL,
		"status":   a.Status,
	}
}

// announcementDeleteAudit is what survives a deletion, and it is the reason a
// sent row is allowed to be deleted at all. Everything announcementAudit
// carries, plus the counts and the date — the statement the frozen row was
// making, written somewhere the delete cannot reach.
func announcementDeleteAudit(a *database.AnnouncementEmail) map[string]any {
	out := announcementAudit(a)
	out["sent_by"] = a.SentByLogin
	out["sent_at"] = a.SentAt
	out["resolved"] = a.Resolved
	out["delivered"] = a.Delivered
	out["failed"] = a.Failed
	out["skipped"] = a.Skipped
	out["abandoned"] = a.Abandoned
	return out
}

func (s *Server) auditAnnouncement(r *http.Request, p *auth.Principal, action string,
	a *database.AnnouncementEmail, after, before string) {
	entry := auditEntryFor(r)
	entry.ActorType = database.PrincipalTypeUser
	entry.ActorID = p.ID
	entry.Action = action
	entry.TargetType = "announcement_email"
	entry.TargetID = a.ID
	entry.After = after
	entry.Before = before
	if err := s.db.WriteAuditRow(entry); err != nil {
		log.Printf("announcement emails: auditing %s on %s: %v", action, a.ID, err)
	}
}

// announcerOrNil returns something that always answers. A Server built without
// an announcer — every test that does not ask for one — reports the install as
// unable to send, which is the same answer an install with no relay gives and
// is the honest one.
func (s *Server) announcerOrNil() Announcer {
	if s.announcer == nil {
		return unavailableAnnouncer{}
	}
	return s.announcer
}

// errNoRelay is what an install with no relay says when something asks it to
// send. Unreachable through the handlers, which check Available() first, and
// present so that the fallback cannot silently succeed.
var errNoRelay = errors.New("email is not configured on this install")

type unavailableAnnouncer struct{}

func (unavailableAnnouncer) Available() bool { return false }
func (unavailableAnnouncer) Start(string)    {}
func (unavailableAnnouncer) SendTest(context.Context, *database.AnnouncementEmail, string) error {
	return errNoRelay
}
