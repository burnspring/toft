package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"toft/internal/database"
)

// The handler half of the announcement: authorization, the caps, the link, and
// the two refusals that have to happen *before* the freeze. Everything after
// the freeze belongs to internal/announce and is tested there.

// fakeAnnouncer stands in for the runner. Start records rather than sends: what
// these tests are about is whether the row was frozen and whether the run was
// handed over at all, and a real relay would only make that slower to assert.
type fakeAnnouncer struct {
	mu        sync.Mutex
	available bool
	started   []string
	tested    []string
	testErr   error
}

func (f *fakeAnnouncer) Available() bool { return f.available }

func (f *fakeAnnouncer) Start(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, id)
}

func (f *fakeAnnouncer) SendTest(_ context.Context, a *database.AnnouncementEmail, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.testErr != nil {
		return f.testErr
	}
	f.tested = append(f.tested, a.ID+" -> "+to)
	return nil
}

func (f *fakeAnnouncer) starts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started)
}

func annEnv(t *testing.T) (*Server, http.Handler, *fakeAnnouncer) {
	t.Helper()
	runner := &fakeAnnouncer{available: true}
	srv := newTestServer(t)
	srv.announcer = runner
	seedUser(t, srv.db, "boss", database.SystemRoleAdmin)
	seedUser(t, srv.db, "ada", database.SystemRolePublisher)
	seedUser(t, srv.db, "bob", database.SystemRoleViewer)
	return srv, srv.Router(http.NotFoundHandler()), runner
}

func annDo(t *testing.T, srv *Server, router http.Handler, who, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Origin", srv.cfg.BaseURL)
	if who != "" {
		req.AddCookie(sessionCookieFor(t, srv, who))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func annCreate(t *testing.T, srv *Server, router http.Handler, body string) string {
	t.Helper()
	rec := annDo(t, srv, router, "boss", http.MethodPost, "/api/v1/admin/announcement-emails", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return got.ID
}

const annBody = `{"subject":"The build queue is down","body":"Back at 14:00.","audience":"publishers"}`

// The existence of this install's announcement history is not owed to a viewer,
// so a non-administrator gets the same answer as somebody asking about a route
// that does not exist.
func TestAnnouncementRoutesAre404ToEveryoneElse(t *testing.T) {
	srv, router, _ := annEnv(t)
	id := annCreate(t, srv, router, annBody)

	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/announcement-emails"},
		{http.MethodPost, "/api/v1/admin/announcement-emails"},
		{http.MethodGet, "/api/v1/admin/announcement-emails/estimate?audience=all"},
		{http.MethodGet, "/api/v1/admin/announcement-emails/" + id},
		{http.MethodPatch, "/api/v1/admin/announcement-emails/" + id},
		{http.MethodDelete, "/api/v1/admin/announcement-emails/" + id},
		{http.MethodPost, "/api/v1/admin/announcement-emails/" + id + "/clone"},
		{http.MethodPost, "/api/v1/admin/announcement-emails/" + id + "/test"},
		{http.MethodPost, "/api/v1/admin/announcement-emails/" + id + "/send"},
		{http.MethodPost, "/api/v1/admin/announcement-emails/" + id + "/resend"},
	} {
		for _, who := range []string{"", "ada", "bob"} {
			rec := annDo(t, srv, router, who, c.method, c.path, "{}")
			if rec.Code != http.StatusNotFound && rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s as %q = %d, want 404", c.method, c.path, who, rec.Code)
			}
		}
	}
}

// The caps refuse the save rather than truncating: a message going out carrying
// words its author never wrote is worse than a refusal.
func TestAnnouncementCapsRefuseRatherThanTruncate(t *testing.T) {
	srv, router, _ := annEnv(t)

	for _, c := range []struct{ name, body, field string }{
		{"no subject", `{"subject":"","body":"x"}`, "subject"},
		{"no body", `{"subject":"x","body":""}`, "body"},
		{"subject too long", `{"subject":"` + strings.Repeat("a", 201) + `","body":"x"}`, "subject"},
		{"body too long", `{"subject":"x","body":"` + strings.Repeat("a", 4001) + `"}`, "body"},
		{"link text too long", `{"subject":"x","body":"y","link_url":"https://e.com","link_text":"` +
			strings.Repeat("a", 61) + `"}`, "link_text"},
		{"unknown audience", `{"subject":"x","body":"y","audience":"editors"}`, "audience"},
		{"link text with no link", `{"subject":"x","body":"y","link_text":"Read it"}`, "link_url"},
	} {
		rec := annDo(t, srv, router, "boss", http.MethodPost, "/api/v1/admin/announcement-emails", c.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400: %s", c.name, rec.Code, rec.Body.String())
			continue
		}
		var got map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if got["field"] != c.field {
			t.Errorf("%s: field = %q, want %q", c.name, got["field"], c.field)
		}
	}
}

// A URL inside mail wearing this install's From address is a phishing
// primitive, so it is refused at the save with the field named.
func TestAnnouncementLinkIsRefusedAtSave(t *testing.T) {
	srv, router, _ := annEnv(t)
	for _, link := range []string{"javascript:alert(1)", "data:text/html,x", "example.com", "https://u:p@e.com"} {
		body, _ := json.Marshal(map[string]string{"subject": "x", "body": "y", "link_url": link})
		rec := annDo(t, srv, router, "boss", http.MethodPost, "/api/v1/admin/announcement-emails", string(body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q was accepted (%d)", link, rec.Code)
			continue
		}
		var got map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if got["field"] != "link_url" {
			t.Errorf("%q: field = %q", link, got["field"])
		}
	}
}

// Both refusals happen before the freeze, because a frozen row that mailed
// nobody is a record of nothing.
func TestSendRefusalsHappenBeforeTheFreeze(t *testing.T) {
	srv, router, runner := annEnv(t)

	// No relay: absent rather than disabled on the surface, and 409 here,
	// because the UI is not the enforcement point.
	runner.available = false
	id := annCreate(t, srv, router, annBody)
	rec := annDo(t, srv, router, "boss", http.MethodPost,
		"/api/v1/admin/announcement-emails/"+id+"/send", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("send with no relay = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	var got map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["code"] != "relay_not_configured" {
		t.Errorf("code = %q", got["code"])
	}
	if a, _ := srv.db.GetAnnouncementEmail(id); a.Status != database.AnnouncementDraft {
		t.Errorf("status = %q after a refused send; it is still a draft", a.Status)
	}
	if runner.starts() != 0 {
		t.Error("a run was started for a send that was refused")
	}
}

// The estimate is what the composer shows, and it is not a freeze.
func TestAnnouncementEstimate(t *testing.T) {
	srv, router, _ := annEnv(t)

	rec := annDo(t, srv, router, "boss", http.MethodGet,
		"/api/v1/admin/announcement-emails/estimate?audience=publishers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("estimate = %d: %s", rec.Code, rec.Body.String())
	}
	var est database.AnnouncementEstimate
	if err := json.Unmarshal(rec.Body.Bytes(), &est); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	// ada, boss and the seeded admin. A viewer is not a publisher.
	if est.Total != 3 || est.Deliverable != 3 {
		t.Errorf("estimate = %+v, want 3 and 3", est)
	}

	rec = annDo(t, srv, router, "boss", http.MethodGet,
		"/api/v1/admin/announcement-emails/estimate?audience=nobody", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown audience was estimated: %d", rec.Code)
	}
}

// One send, whoever presses first. A regression here double-mails the install
// and nothing on the platform reports it.
func TestSendFreezesOnceAndHandsOver(t *testing.T) {
	srv, router, runner := annEnv(t)
	id := annCreate(t, srv, router, annBody)

	const racers = 6
	var wg sync.WaitGroup
	codes := make([]int, racers)
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = annDo(t, srv, router, "boss", http.MethodPost,
				"/api/v1/admin/announcement-emails/"+id+"/send", "").Code
		}()
	}
	wg.Wait()

	accepted, conflicts := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusAccepted:
			accepted++
		case http.StatusConflict:
			conflicts++
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	if accepted != 1 || conflicts != racers-1 {
		t.Fatalf("%d accepted and %d refused of %d; exactly one may send", accepted, conflicts, racers)
	}
	if runner.starts() != 1 {
		t.Errorf("the runner was started %d times", runner.starts())
	}

	// And the frozen row refuses edits, with the status on the refusal.
	rec := annDo(t, srv, router, "boss", http.MethodPatch,
		"/api/v1/admin/announcement-emails/"+id, `{"subject":"Rewritten"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("PATCH on a frozen row = %d, want 409", rec.Code)
	}
	var got map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["status"] == "" {
		t.Error("the refusal does not say what the status is")
	}
}

// A draft deletes, a completed record deletes, and a run in flight does not —
// the last with its own code, because the surface offers the delete again the
// moment the run finishes.
func TestDeleteTakesDraftsAndRecordsButNotARunInFlight(t *testing.T) {
	srv, router, _ := annEnv(t)
	id := annCreate(t, srv, router, annBody)
	if rec := annDo(t, srv, router, "boss", http.MethodDelete,
		"/api/v1/admin/announcement-emails/"+id, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("deleting a draft = %d", rec.Code)
	}
	if a, _ := srv.db.GetAnnouncementEmail(id); a != nil {
		t.Error("the draft is still there")
	}

	// A second one, frozen. The fake runner never completes it, so the row sits
	// in `sending` — which is exactly the state the delete refuses.
	id = annCreate(t, srv, router, annBody)
	if rec := annDo(t, srv, router, "boss", http.MethodPost,
		"/api/v1/admin/announcement-emails/"+id+"/send", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("send = %d: %s", rec.Code, rec.Body.String())
	}
	rec := annDo(t, srv, router, "boss", http.MethodDelete,
		"/api/v1/admin/announcement-emails/"+id, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("deleting a run in flight = %d, want 409", rec.Code)
	}
	var got map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["code"] != "send_in_flight" || got["status"] != database.AnnouncementSending {
		t.Errorf("the refusal reads %v; it has to name the code and the status", got)
	}

	// Once the run has completed, the record is the administrator's to be rid
	// of — and what is left behind is the audit row, carrying the counts.
	if err := srv.db.FinishAnnouncementSend(id); err != nil {
		t.Fatalf("FinishAnnouncementSend: %v", err)
	}
	if rec := annDo(t, srv, router, "boss", http.MethodDelete,
		"/api/v1/admin/announcement-emails/"+id, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("deleting a sent record = %d: %s", rec.Code, rec.Body.String())
	}
	if a, _ := srv.db.GetAnnouncementEmail(id); a != nil {
		t.Error("the record is still there")
	}
	var before string
	if err := srv.db.Conn().QueryRow(
		`SELECT COALESCE(before::text,'') FROM audit_log
		 WHERE action = 'announcement_email.delete' AND target_id = ?`, id).Scan(&before); err != nil {
		t.Fatalf("reading the delete row: %v", err)
	}
	for _, want := range []string{"subject", "audience", "resolved", "delivered", "failed"} {
		if !strings.Contains(before, want) {
			t.Errorf("the delete's audit row does not carry %q: %s", want, before)
		}
	}
}

// Clone copies the content and the audience, and nothing else.
func TestCloneCopiesContentNotTheRecord(t *testing.T) {
	srv, router, _ := annEnv(t)
	id := annCreate(t, srv, router,
		`{"subject":"Postgres upgrade","body":"Saturday.","audience":"publishers",`+
			`"link_url":"https://example.com/plan","link_text":"Read the plan"}`)
	if rec := annDo(t, srv, router, "boss", http.MethodPost,
		"/api/v1/admin/announcement-emails/"+id+"/send", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("send = %d: %s", rec.Code, rec.Body.String())
	}

	rec := annDo(t, srv, router, "boss", http.MethodPost,
		"/api/v1/admin/announcement-emails/"+id+"/clone", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("clone = %d: %s", rec.Code, rec.Body.String())
	}
	var clone database.AnnouncementEmail
	if err := json.Unmarshal(rec.Body.Bytes(), &clone); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if clone.Status != database.AnnouncementDraft || clone.SentAt != nil || clone.Resolved != 0 {
		t.Errorf("the clone carried the record over: %+v", clone)
	}
	if clone.ClonedFrom != id {
		t.Errorf("cloned_from = %q, want %q", clone.ClonedFrom, id)
	}
	if clone.Subject != "Postgres upgrade" || clone.LinkText != "Read the plan" {
		t.Errorf("the clone lost content: %+v", clone)
	}
}

// The test send goes to the composing administrator, records nothing, and says
// so when there is nowhere to send it.
func TestTestSendGoesToTheComposer(t *testing.T) {
	srv, router, runner := annEnv(t)
	id := annCreate(t, srv, router, annBody)

	rec := annDo(t, srv, router, "boss", http.MethodPost,
		"/api/v1/admin/announcement-emails/"+id+"/test", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("test = %d: %s", rec.Code, rec.Body.String())
	}
	if len(runner.tested) != 1 || !strings.HasSuffix(runner.tested[0], "boss@example.com") {
		t.Errorf("tested = %v", runner.tested)
	}
	a, _ := srv.db.GetAnnouncementEmail(id)
	if a.Status != database.AnnouncementDraft || a.Resolved != 0 {
		t.Errorf("a test send moved the record: %q resolved=%d", a.Status, a.Resolved)
	}
	rows, _ := srv.db.ListAnnouncementRecipients(id)
	if len(rows) != 0 {
		t.Errorf("a test send wrote %d recipient row(s)", len(rows))
	}

	// An account with no address on file — the first time most administrators
	// meet the skip rule, and the right place to meet it.
	if _, err := srv.db.Conn().Exec(`UPDATE users SET email = '' WHERE id = 'boss'`); err != nil {
		t.Fatalf("clearing the address: %v", err)
	}
	rec = annDo(t, srv, router, "boss", http.MethodPost,
		"/api/v1/admin/announcement-emails/"+id+"/test", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("test with no address = %d, want 409", rec.Code)
	}
	var got map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["code"] != "no_address" {
		t.Errorf("code = %q", got["code"])
	}
}

// Five actions, and every one of them lands in the settings family the audit
// filter already offers.
func TestAuditRowsForEveryTransition(t *testing.T) {
	srv, router, _ := annEnv(t)
	id := annCreate(t, srv, router, annBody)
	annDo(t, srv, router, "boss", http.MethodPatch,
		"/api/v1/admin/announcement-emails/"+id, `{"body":"Back at 15:00."}`)
	annDo(t, srv, router, "boss", http.MethodPost,
		"/api/v1/admin/announcement-emails/"+id+"/test", "")
	annDo(t, srv, router, "boss", http.MethodPost,
		"/api/v1/admin/announcement-emails/"+id+"/send", "")

	other := annCreate(t, srv, router, annBody)
	annDo(t, srv, router, "boss", http.MethodDelete, "/api/v1/admin/announcement-emails/"+other, "")

	for _, action := range []string{
		"announcement_email.create", "announcement_email.update",
		"announcement_email.test", "announcement_email.send", "announcement_email.delete",
	} {
		var n int
		if err := srv.db.Conn().QueryRow(
			`SELECT COUNT(*) FROM audit_log WHERE action = ?`, action).Scan(&n); err != nil {
			t.Fatalf("counting %s: %v", action, err)
		}
		if n == 0 {
			t.Errorf("%s wrote no audit row", action)
		}
	}

	// The send row carries the audience, the count and the subject — never the
	// body, which is on the row itself and is not what an audit reader scans
	// for.
	var after string
	if err := srv.db.Conn().QueryRow(
		`SELECT COALESCE(after::text,'') FROM audit_log WHERE action = 'announcement_email.send'`).
		Scan(&after); err != nil {
		t.Fatalf("reading the send row: %v", err)
	}
	for _, want := range []string{"audience", "resolved", "subject"} {
		if !strings.Contains(after, want) {
			t.Errorf("the send audit row does not carry %q: %s", want, after)
		}
	}
	if strings.Contains(after, "Back at 15:00") {
		t.Error("the send audit row carries the body")
	}
}

// With no runner at all — every Server built without one — the install reports
// itself unable to send rather than panicking.
func TestNoAnnouncerReportsUnavailable(t *testing.T) {
	srv := newTestServer(t)
	seedUser(t, srv.db, "boss", database.SystemRoleAdmin)
	router := srv.Router(http.NotFoundHandler())

	rec := annDo(t, srv, router, "boss", http.MethodGet, "/api/v1/admin/announcement-emails", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		CanSend bool `json:"can_send"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got.CanSend {
		t.Error("an install with no runner reports that it can send")
	}
}

// The resend's refusals all come before a row exists, and the one that gets
// through freezes a new row and hands it to the runner — once.
func TestResendRefusalsAndHandover(t *testing.T) {
	srv, router, runner := annEnv(t)
	id := annCreate(t, srv, router, annBody)
	resend := "/api/v1/admin/announcement-emails/" + id + "/resend"

	code := func(rec *httptest.ResponseRecorder) string {
		var got map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		return got["code"]
	}

	if rec := annDo(t, srv, router, "boss", http.MethodPost, resend, ""); rec.Code != http.StatusConflict || code(rec) != "not_sent" {
		t.Fatalf("resend of a draft = %d %s, want 409 not_sent", rec.Code, rec.Body.String())
	}

	// Sent, and the fake runner sends nothing: resolve and abandon by hand,
	// which is what a restart mid-run leaves.
	if rec := annDo(t, srv, router, "boss", http.MethodPost, "/api/v1/admin/announcement-emails/"+id+"/send", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("send = %d", rec.Code)
	}
	if rec := annDo(t, srv, router, "boss", http.MethodPost, resend, ""); rec.Code != http.StatusConflict || code(rec) != "send_in_flight" {
		t.Fatalf("resend of a run in flight = %d %s, want 409 send_in_flight", rec.Code, rec.Body.String())
	}
	if _, err := srv.db.ResolveAnnouncementRecipients(id, database.AnnouncementAudiencePublishers); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.db.AbandonAnnouncementSend(id); err != nil {
		t.Fatal(err)
	}

	runner.available = false
	if rec := annDo(t, srv, router, "boss", http.MethodPost, resend, ""); rec.Code != http.StatusConflict || code(rec) != "relay_not_configured" {
		t.Fatalf("resend with no relay = %d, want 409 relay_not_configured", rec.Code)
	}
	runner.available = true

	startsBefore := runner.starts()
	rec := annDo(t, srv, router, "boss", http.MethodPost, resend, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("resend = %d %s, want 202", rec.Code, rec.Body.String())
	}
	var created database.AnnouncementEmail
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.ResendOf != id || created.Status != database.AnnouncementSending {
		t.Errorf("resend row = %+v", created)
	}
	if runner.starts() != startsBefore+1 || runner.started[len(runner.started)-1] != created.ID {
		t.Errorf("the runner was not handed the resend")
	}

	rec = annDo(t, srv, router, "boss", http.MethodPost, resend, "")
	var refused map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &refused)
	if rec.Code != http.StatusConflict || refused["code"] != "already_resent" || refused["resent_as"] != created.ID {
		t.Errorf("second resend = %d %v, want 409 already_resent naming %s", rec.Code, refused, created.ID)
	}

	// A sent announcement that reached everybody has nobody to resend to.
	other := annCreate(t, srv, router, annBody)
	if ok, err := srv.db.BeginAnnouncementSend(other, ""); err != nil || !ok {
		t.Fatal(err)
	}
	if err := srv.db.FinishAnnouncementSend(other); err != nil {
		t.Fatal(err)
	}
	if rec := annDo(t, srv, router, "boss", http.MethodPost, "/api/v1/admin/announcement-emails/"+other+"/resend", ""); rec.Code != http.StatusConflict || code(rec) != "nothing_to_resend" {
		t.Errorf("resend with nobody missed = %d %s, want 409 nothing_to_resend", rec.Code, rec.Body.String())
	}
}
