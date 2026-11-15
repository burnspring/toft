package announce

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/database/dbtest"
	"toft/internal/mail"
)

// fakeRelay answers however the test tells it to, per address. It is the only
// way the interesting half of this package can be asserted at all: a relay that
// rate-limits at recipient forty is not something a real one will do on demand.
type fakeRelay struct {
	mu sync.Mutex
	// failures[addr] is how many times this address fails before succeeding.
	// A missing entry succeeds immediately; a negative one always fails.
	failures map[string]int
	sent     []mail.Message
	err      error
}

func (f *fakeRelay) SendOne(_ context.Context, msg mail.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	to := msg.To[0]
	if n, ok := f.failures[to]; ok && n != 0 {
		if n > 0 {
			f.failures[to] = n - 1
		}
		if f.err != nil {
			return f.err
		}
		return errors.New("451 4.7.1 try again later")
	}
	f.sent = append(f.sent, msg)
	return nil
}

func (f *fakeRelay) BaseURL() string    { return "https://toft.internal" }
func (f *fakeRelay) SenderName() string { return "Toft" }
func (f *fakeRelay) Enabled() bool      { return true }

func newRunner(db *database.DB, relay *fakeRelay) *Runner {
	r := New(db, relay)
	// The schedule is the subject of one test and an obstacle in every other.
	r.pace = 0
	r.backoff = []time.Duration{0, 0, 0}
	return r
}

func mkUser(t *testing.T, db *database.DB, login, role, email string) {
	t.Helper()
	if _, err := db.Conn().Exec(`
		INSERT INTO users (id, login, name, email, avatar_url, system_role, created_at)
		VALUES (?, ?, '', ?, '', ?, NOW())`, "u-"+login, login, email, role); err != nil {
		t.Fatalf("creating %s: %v", login, err)
	}
}

// frozen writes a draft and freezes it, which is what the handler does before
// it starts the runner.
func frozen(t *testing.T, db *database.DB, a *database.AnnouncementEmail) *database.AnnouncementEmail {
	t.Helper()
	if a.Subject == "" {
		a.Subject = "The build queue is down"
	}
	if a.Body == "" {
		a.Body = "Back at 14:00."
	}
	if a.Audience == "" {
		a.Audience = database.AnnouncementAudiencePublishers
	}
	if err := db.CreateAnnouncementEmail(a); err != nil {
		t.Fatalf("CreateAnnouncementEmail: %v", err)
	}
	ok, err := db.BeginAnnouncementSend(a.ID, "")
	if err != nil || !ok {
		t.Fatalf("BeginAnnouncementSend: ok=%v err=%v", ok, err)
	}
	return a
}

func reread(t *testing.T, db *database.DB, id string) *database.AnnouncementEmail {
	t.Helper()
	a, err := db.GetAnnouncementEmail(id)
	if err != nil {
		t.Fatalf("GetAnnouncementEmail: %v", err)
	}
	return a
}

// One run, one message per person, and the counts at the end are the rows.
func TestRunDeliversOnePerRecipient(t *testing.T) {
	db := dbtest.New(t)
	mkUser(t, db, "ada", database.SystemRolePublisher, "ada@example.com")
	mkUser(t, db, "bob", database.SystemRolePublisher, "bob@example.com")
	mkUser(t, db, "cleo", database.SystemRoleViewer, "cleo@example.com")

	relay := &fakeRelay{failures: map[string]int{}}
	a := frozen(t, db, &database.AnnouncementEmail{Audience: database.AnnouncementAudiencePublishers})
	if err := newRunner(db, relay).Run(context.Background(), a.ID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// ada, bob and the seeded admin. A viewer is not a publisher.
	if len(relay.sent) != 3 {
		t.Fatalf("%d messages sent, want 3", len(relay.sent))
	}
	seen := map[string]bool{}
	for _, m := range relay.sent {
		if len(m.To) != 1 {
			t.Errorf("a message carried %d addresses; a To: with the directory in it is a disclosure nobody declared", len(m.To))
		}
		if len(m.CC) != 0 || len(m.BCC) != 0 {
			t.Errorf("a message carried CC or BCC")
		}
		if m.Audience != mail.AudienceAnnouncement {
			t.Errorf("message audience = %q", m.Audience)
		}
		seen[m.To[0]] = true
	}
	if !seen["ada@example.com"] || !seen["bob@example.com"] {
		t.Errorf("recipients were %v", seen)
	}
	if seen["cleo@example.com"] {
		t.Error("a viewer was mailed an announcement addressed to publishers")
	}

	got := reread(t, db, a.ID)
	if got.Status != database.AnnouncementSent {
		t.Errorf("status = %q, want sent", got.Status)
	}
	if got.Delivered != 3 || got.Failed != 0 || got.Resolved != 3 {
		t.Errorf("counts = resolved %d delivered %d failed %d", got.Resolved, got.Delivered, got.Failed)
	}
	if got.CompletedAt == nil {
		t.Error("completed_at was not stamped")
	}
}

// The one thing this send path adds that SendAsync refuses: one run's own
// patience with a relay that rate-limited it.
func TestRetryThenFail(t *testing.T) {
	db := dbtest.New(t)
	mkUser(t, db, "ada", database.SystemRolePublisher, "ada@example.com")
	mkUser(t, db, "bob", database.SystemRolePublisher, "bob@example.com")

	relay := &fakeRelay{failures: map[string]int{
		// Fails twice, then answers.
		"ada@example.com": 2,
		// Fails every attempt.
		"bob@example.com": -1,
	}}
	a := frozen(t, db, &database.AnnouncementEmail{})
	if err := newRunner(db, relay).Run(context.Background(), a.ID); err != nil {
		t.Fatalf("Run: %v", err)
	}

	rows, err := db.ListAnnouncementRecipients(a.ID)
	if err != nil {
		t.Fatalf("ListAnnouncementRecipients: %v", err)
	}
	byLogin := map[string]database.AnnouncementRecipient{}
	for _, r := range rows {
		byLogin[r.Login] = r
	}
	if ada := byLogin["ada"]; ada.Status != database.AnnouncementDelivered || ada.Attempts != 3 {
		t.Errorf("ada: %q after %d attempts; a relay that failed twice then answered delivers on the third",
			ada.Status, ada.Attempts)
	}
	bob := byLogin["bob"]
	if bob.Status != database.AnnouncementFailed {
		t.Errorf("bob: %q, want failed", bob.Status)
	}
	if bob.Attempts != 4 {
		t.Errorf("bob was tried %d times, want 4 — one attempt and three retries", bob.Attempts)
	}
	if !strings.Contains(bob.Reason, "451") {
		t.Errorf("bob's reason is %q; the relay's own words are what an administrator chasing a bounce needs", bob.Reason)
	}

	got := reread(t, db, a.ID)
	if got.Status != database.AnnouncementSent {
		t.Errorf("status = %q — sent is terminal whatever the outcomes were", got.Status)
	}
	if got.Failed != 1 || got.Delivered != 2 {
		t.Errorf("counts = delivered %d failed %d, want 2 and 1", got.Delivered, got.Failed)
	}
}

// A run that cannot start still completes the row, with every recipient
// carrying the reason. The alternative is a row stuck in `sending` forever,
// which is a status meaning "we do not know".
func TestRefusedLinkFailsEveryRecipientAndCompletes(t *testing.T) {
	db := dbtest.New(t)
	mkUser(t, db, "ada", database.SystemRolePublisher, "ada@example.com")

	relay := &fakeRelay{failures: map[string]int{}}
	a := frozen(t, db, &database.AnnouncementEmail{LinkURL: "javascript:alert(1)"})
	if err := newRunner(db, relay).Run(context.Background(), a.ID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(relay.sent) != 0 {
		t.Fatalf("%d messages went out behind a refused link", len(relay.sent))
	}
	got := reread(t, db, a.ID)
	if got.Status != database.AnnouncementSent || got.Failed != 2 || got.Delivered != 0 {
		t.Errorf("after a refused link: %q delivered=%d failed=%d", got.Status, got.Delivered, got.Failed)
	}
	rows, err := db.ListAnnouncementRecipients(a.ID)
	if err != nil {
		t.Fatalf("ListAnnouncementRecipients: %v", err)
	}
	for _, r := range rows {
		if !strings.Contains(r.Reason, "link") {
			t.Errorf("%s was failed for %q, which does not say why", r.Login, r.Reason)
		}
	}
}

// The freeze decides who runs. A second runner on the same row sends nothing.
func TestRunRefusesARowItDidNotFreeze(t *testing.T) {
	db := dbtest.New(t)
	mkUser(t, db, "ada", database.SystemRolePublisher, "ada@example.com")
	relay := &fakeRelay{failures: map[string]int{}}
	runner := newRunner(db, relay)

	a := frozen(t, db, &database.AnnouncementEmail{})
	if err := runner.Run(context.Background(), a.ID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	first := len(relay.sent)
	if first == 0 {
		t.Fatal("the first run sent nothing")
	}
	if err := runner.Run(context.Background(), a.ID); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(relay.sent) != first {
		t.Errorf("a second run of a sent announcement mailed %d more people", len(relay.sent)-first)
	}

	draft := &database.AnnouncementEmail{Subject: "Not frozen", Body: "x", Audience: database.AnnouncementAudienceAll}
	if err := db.CreateAnnouncementEmail(draft); err != nil {
		t.Fatalf("CreateAnnouncementEmail: %v", err)
	}
	if err := runner.Run(context.Background(), draft.ID); err != nil {
		t.Fatalf("Run on a draft: %v", err)
	}
	if len(relay.sent) != first {
		t.Error("a draft was sent by a runner nobody handed a frozen row to")
	}
}

// The URL that will wear this install's From address.
func TestLinkValidation(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"", true},
		{"https://toft.internal/notice", true},
		{"http://example.com", true},
		{"javascript:alert(1)", false},
		{"data:text/html,<script>alert(1)</script>", false},
		{"file:///etc/passwd", false},
		{"example.com/notice", false},
		{"http:///notice", false},
		{"https://user:pass@example.com", false},
		{"https://" + strings.Repeat("a", MaxLinkURL) + ".example.com", false},
	} {
		err := ValidateLink(tc.raw)
		if (err == nil) != tc.want {
			t.Errorf("ValidateLink(%q) = %v, want accepted=%v", tc.raw, err, tc.want)
		}
	}
}

// The record the composer's test send previews is the one that goes out.
func TestRenderCarriesTheAdministratorsWords(t *testing.T) {
	msg, err := Render("https://toft.internal", "", &database.AnnouncementEmail{
		Subject:  "Postgres upgrade on Saturday",
		Body:     "Expect an hour of downtime from 09:00.",
		LinkURL:  "https://toft.internal/status",
		LinkText: "Read the plan",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if msg.Subject != "Postgres upgrade on Saturday" {
		t.Errorf("subject = %q", msg.Subject)
	}
	for _, want := range []string{"Expect an hour of downtime", "Read the plan", "toft.internal/status"} {
		if !strings.Contains(msg.HTML, want) {
			t.Errorf("the HTML part is missing %q", want)
		}
		if !strings.Contains(msg.Text, want) {
			t.Errorf("the text part is missing %q", want)
		}
	}
	if msg.Audience != mail.AudienceAnnouncement {
		t.Errorf("audience = %q", msg.Audience)
	}
}

// A resend reaches exactly who the run missed — its failed and abandoned
// people, read back through their accounts as they are now — and leaves the
// original's record exactly as it was.
func TestResendReachesOnlyWhoTheRunMissed(t *testing.T) {
	db := dbtest.New(t)
	mkUser(t, db, "ada", database.SystemRolePublisher, "ada@old.example.com")
	mkUser(t, db, "bob", database.SystemRolePublisher, "bob@example.com")
	mkUser(t, db, "cleo", database.SystemRolePublisher, "cleo@example.com")
	mkUser(t, db, "dev", database.SystemRolePublisher, "dev@example.com")

	relay := &fakeRelay{failures: map[string]int{
		"ada@old.example.com": -1,
		"cleo@example.com":    -1,
		"dev@example.com":     -1,
	}}
	a := frozen(t, db, &database.AnnouncementEmail{})
	if err := newRunner(db, relay).Run(context.Background(), a.ID); err != nil {
		t.Fatalf("Run: %v", err)
	}
	before := reread(t, db, a.ID)
	if before.Failed != 3 {
		t.Fatalf("failed = %d, want 3", before.Failed)
	}

	// Since the run: ada's address was fixed, cleo was locked, dev was deleted.
	if _, err := db.Conn().Exec(`UPDATE users SET email = 'ada@new.example.com' WHERE login = 'ada'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Conn().Exec(`UPDATE users SET locked_at = NOW() WHERE login = 'cleo'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Conn().Exec(`DELETE FROM users WHERE login = 'dev'`); err != nil {
		t.Fatal(err)
	}

	if n, err := db.CountAnnouncementResendable(a.ID); err != nil || n != 2 {
		t.Fatalf("resendable = %d (%v), want 2: ada and cleo, not the deleted dev", n, err)
	}
	resend, err := db.CreateAnnouncementResend(a.ID, "")
	if err != nil || resend == nil {
		t.Fatalf("CreateAnnouncementResend: %v %v", resend, err)
	}
	if resend.Status != database.AnnouncementSending || resend.ResendOf != a.ID ||
		resend.Subject != a.Subject || resend.Body != a.Body {
		t.Fatalf("resend = %+v", resend)
	}
	if _, err := db.CreateAnnouncementResend(a.ID, ""); !errors.Is(err, database.ErrAlreadyResent) {
		t.Errorf("a second resend: err = %v, want ErrAlreadyResent", err)
	}

	relay.sent = nil
	relay.failures = map[string]int{}
	if err := newRunner(db, relay).Run(context.Background(), resend.ID); err != nil {
		t.Fatalf("Run resend: %v", err)
	}
	if len(relay.sent) != 1 || relay.sent[0].To[0] != "ada@new.example.com" {
		var to []string
		for _, m := range relay.sent {
			to = append(to, m.To...)
		}
		t.Fatalf("the resend mailed %v, want only ada at her new address", to)
	}
	got := reread(t, db, resend.ID)
	if got.Status != database.AnnouncementSent || got.Resolved != 2 ||
		got.Delivered != 1 || got.Skipped != 1 {
		t.Errorf("resend counts = resolved %d delivered %d skipped %d, status %s",
			got.Resolved, got.Delivered, got.Skipped, got.Status)
	}

	after := reread(t, db, a.ID)
	if after.ResentAs != resend.ID {
		t.Errorf("original resent_as = %q, want %q", after.ResentAs, resend.ID)
	}
	if after.Delivered != before.Delivered || after.Failed != before.Failed || after.Status != before.Status {
		t.Error("resending moved the original's record")
	}
}

// Abandoned is the other half of "did not reach": a run a restart cut short is
// resendable, and an announcement that is not sent cannot be resent.
func TestResendTakesAbandonedAndOnlySentRows(t *testing.T) {
	db := dbtest.New(t)
	mkUser(t, db, "ada", database.SystemRolePublisher, "ada@example.com")

	draft := &database.AnnouncementEmail{Subject: "s", Body: "b", Audience: database.AnnouncementAudiencePublishers}
	if err := db.CreateAnnouncementEmail(draft); err != nil {
		t.Fatal(err)
	}
	if got, err := db.CreateAnnouncementResend(draft.ID, ""); err != nil || got != nil {
		t.Errorf("resend of a draft = %v, %v; want nil, nil", got, err)
	}

	a := frozen(t, db, &database.AnnouncementEmail{})
	if _, err := db.ResolveAnnouncementRecipients(a.ID, a.Audience); err != nil {
		t.Fatal(err)
	}
	if got, err := db.CreateAnnouncementResend(a.ID, ""); err != nil || got != nil {
		t.Errorf("resend of a run in flight = %v, %v; want nil, nil", got, err)
	}
	if _, err := db.AbandonAnnouncementSend(a.ID); err != nil {
		t.Fatal(err)
	}
	// ada and the seeded admin, both abandoned.
	if n, err := db.CountAnnouncementResendable(a.ID); err != nil || n != 2 {
		t.Fatalf("resendable = %d (%v), want 2", n, err)
	}
	resend, err := db.CreateAnnouncementResend(a.ID, "")
	if err != nil || resend == nil {
		t.Fatalf("CreateAnnouncementResend: %v %v", resend, err)
	}
	relay := &fakeRelay{failures: map[string]int{}}
	if err := newRunner(db, relay).Run(context.Background(), resend.ID); err != nil {
		t.Fatal(err)
	}
	if len(relay.sent) != 2 {
		t.Errorf("the resend mailed %d, want 2", len(relay.sent))
	}
}
