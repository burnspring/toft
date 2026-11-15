package events

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"toft/internal/database"
	mailer "toft/internal/mail"
)

// fakeStore is the whole Store interface in memory, with the same collapsing
// rule the real ClaimEventNotify enforces in SQL. It exists so these tests pin
// the recorder's decisions rather than Postgres's.
type fakeStore struct {
	events    []*database.AppEvent
	notifyOn  bool
	people    []database.Recipient
	app       *database.App
	insertErr error
	nextID    int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		notifyOn: true,
		app:      &database.App{ID: "app-1", Slug: "reports", Name: "Quarterly Report"},
		people:   []database.Recipient{{Address: "ada@example.com", DisplayName: "ada"}},
	}
}

func (f *fakeStore) InsertAppEvent(e *database.AppEvent) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	f.nextID++
	e.ID = string(rune('a' + f.nextID))
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	copied := *e
	f.events = append(f.events, &copied)
	return nil
}

func (f *fakeStore) ClaimEventNotify(eventID, appID, kind, fingerprint string, cooldown time.Duration) (bool, error) {
	cutoff := time.Now().UTC().Add(-cooldown)
	var target *database.AppEvent
	claimed := 0
	for _, e := range f.events {
		if e.ID == eventID {
			target = e
		}
		if e.AppID == appID && e.Kind == kind && e.Fingerprint == fingerprint &&
			e.NotifiedAt != nil && e.NotifiedAt.After(cutoff) {
			// The store's two guards: a claimed occurrence still open, or two
			// claimed occurrences in the window.
			if e.ResolvedAt == nil {
				return false, nil
			}
			claimed++
		}
	}
	if claimed >= 2 {
		return false, nil
	}
	if target == nil || target.NotifiedAt != nil {
		return false, nil
	}
	now := time.Now().UTC()
	target.NotifiedAt = &now
	return true, nil
}

func (f *fakeStore) MarkEventNoticeSent(eventID string) error {
	for _, e := range f.events {
		if e.ID == eventID {
			e.NoticeSent = true
		}
	}
	return nil
}

func (f *fakeStore) ResolveAppEvents(appID, kind, prefix string) (int, error) {
	notified := 0
	now := time.Now().UTC()
	for _, e := range f.events {
		if e.AppID == appID && e.Kind == kind && e.ResolvedAt == nil &&
			strings.HasPrefix(e.Fingerprint, prefix) {
			e.ResolvedAt = &now
			// notice_sent, not notified_at: holding the claim is not the same
			// as anybody having heard about it.
			if e.NoticeSent {
				notified++
			}
		}
	}
	return notified, nil
}

func (f *fakeStore) AppNotifyFailures(appID string) (bool, error) { return f.notifyOn, nil }
func (f *fakeStore) ListAppRecipients(appID string) ([]database.Recipient, error) {
	return f.people, nil
}
func (f *fakeStore) GetApp(id string) (*database.App, error) { return f.app, nil }

type fakeMailer struct{ sent []mailer.Message }

func (m *fakeMailer) SendAsync(msg mailer.Message) { m.sent = append(m.sent, msg) }
func (m *fakeMailer) BaseURL() string              { return "https://toft.internal" }

func newRecorder(t *testing.T) (*Recorder, *fakeStore, *fakeMailer) {
	t.Helper()
	store, mail := newFakeStore(), &fakeMailer{}
	r := NewRecorder(store, mail, nil)
	return r, store, mail
}

func ev(kind Kind, fingerprint string) Event {
	return Event{
		AppID: "app-1", Kind: kind, Source: database.EventSourceAutodeploy,
		Summary: "the access token is no longer accepted", Fingerprint: fingerprint,
	}
}

// The first occurrence mails; the second inside the window does not, but is
// still recorded. That asymmetry is the whole design.
func TestRecordCollapsesMailNotRows(t *testing.T) {
	r, store, mail := newRecorder(t)

	r.Record(ev(KindGitAuthFailed, "auth"))
	if len(mail.sent) != 1 {
		t.Fatalf("first occurrence sent %d messages, want 1", len(mail.sent))
	}

	r.Record(ev(KindGitAuthFailed, "auth"))
	r.Record(ev(KindGitAuthFailed, "auth"))
	if len(mail.sent) != 1 {
		t.Fatalf("repeats sent %d messages, want 1", len(mail.sent))
	}
	if len(store.events) != 3 {
		t.Fatalf("recorded %d rows, want 3 — every occurrence is a row", len(store.events))
	}

	// A different fingerprint is a different problem.
	r.Record(ev(KindGitAuthFailed, "network"))
	if len(mail.sent) != 2 {
		t.Fatalf("a new fingerprint sent %d messages, want 2", len(mail.sent))
	}

	// And so is a different kind.
	r.Record(ev(KindAppCrashed, "auth"))
	if len(mail.sent) != 3 {
		t.Fatalf("a new kind sent %d messages, want 3", len(mail.sent))
	}
}

// Once the window passes, the same problem is raised again.
func TestRecordMailsAgainAfterCooldown(t *testing.T) {
	r, _, mail := newRecorder(t)
	r.SetCooldown(time.Nanosecond)

	r.Record(ev(KindGitAuthFailed, "auth"))
	time.Sleep(2 * time.Millisecond)
	r.Record(ev(KindGitAuthFailed, "auth"))
	if len(mail.sent) != 2 {
		t.Fatalf("sent %d messages across an expired cooldown, want 2", len(mail.sent))
	}
}

// The per-app switch mutes mail and never the record. It also must not bank a
// backlog: turning it back on releases nothing, because the claim was taken
// while it was off.
func TestNotifyFailuresOffRecordsButDoesNotSend(t *testing.T) {
	r, store, mail := newRecorder(t)
	store.notifyOn = false

	r.Record(ev(KindGitAuthFailed, "auth"))
	if len(mail.sent) != 0 {
		t.Fatalf("sent %d messages with notifications off, want 0", len(mail.sent))
	}
	if len(store.events) != 1 {
		t.Fatalf("recorded %d rows with notifications off, want 1", len(store.events))
	}
	if store.events[0].NotifiedAt == nil {
		t.Fatal("the claim was not taken, so turning the switch back on would release a backlog")
	}

	store.notifyOn = true
	r.Record(ev(KindGitAuthFailed, "auth"))
	if len(mail.sent) != 0 {
		t.Fatalf("turning notifications on released %d suppressed messages, want 0", len(mail.sent))
	}
}

// BB-97: the switch is about failures. A success the app asked to be told
// about (a job's notify = "always") still goes; a failure through Notify does not.
func TestNotifyFailuresOffLetsARequestedSuccessThrough(t *testing.T) {
	r, store, mail := newRecorder(t)
	store.notifyOn = false

	if r.Notify("app_event", "app-1", "The job failed", "", database.EventSeverityError) || len(mail.sent) != 0 {
		t.Fatalf("a failure went out with the switch off (%d)", len(mail.sent))
	}
	if !r.NotifyRequested("app_event", "app-1", "The job succeeded", "", database.EventSeverityInfo) || len(mail.sent) != 1 {
		t.Fatalf("a requested success was silenced by the failure switch (%d)", len(mail.sent))
	}
}

// Clear sends exactly one recovery notice, and only if something was mailed.
func TestClearSendsOneRecoveryAndOnlyIfMailed(t *testing.T) {
	r, store, mail := newRecorder(t)

	// Nothing recorded at all: nothing to say.
	r.Clear("app-1", KindGitAuthFailed)
	if len(mail.sent) != 0 {
		t.Fatalf("cleared a problem that never happened and sent %d messages", len(mail.sent))
	}

	// Recorded but never mailed (notifications off): still nothing to say.
	store.notifyOn = false
	r.Record(ev(KindJobRunFailed, "job-1"))
	store.notifyOn = true
	r.Clear("app-1", KindJobRunFailed)
	if len(mail.sent) != 0 {
		t.Fatalf("sent a recovery for a problem nobody was told about (%d messages)", len(mail.sent))
	}

	// Recorded and mailed: one recovery, once.
	r.Record(ev(KindGitAuthFailed, "auth"))
	if len(mail.sent) != 1 {
		t.Fatalf("setup: expected the failure mail, got %d", len(mail.sent))
	}
	r.Clear("app-1", KindGitAuthFailed)
	if len(mail.sent) != 2 {
		t.Fatalf("sent %d messages after clearing, want 2", len(mail.sent))
	}
	if !strings.Contains(mail.sent[1].Subject, "Recovered") {
		t.Fatalf("recovery subject = %q", mail.sent[1].Subject)
	}
	r.Clear("app-1", KindGitAuthFailed)
	if len(mail.sent) != 2 {
		t.Fatalf("clearing twice sent %d messages, want 2", len(mail.sent))
	}
}

// A cleared problem that comes back is a new problem and mails again, even
// inside what would otherwise be the cooldown.
func TestClearedProblemMailsAgainWhenItReturns(t *testing.T) {
	r, _, mail := newRecorder(t)

	r.Record(ev(KindAppCrashed, "rel-1"))
	r.Clear("app-1", KindAppCrashed)
	r.Record(ev(KindAppCrashed, "rel-1"))
	// Failure, recovery, failure again: the owners' last word must not be
	// "working again" while the app is broken (VN-3, 2026-11-15).
	if len(mail.sent) != 3 {
		t.Fatalf("sent %d messages, want 3 (failure + recovery + failure)", len(mail.sent))
	}

	// But the window is bounded: a second recovery is told, and a third
	// failure inside the same window is not, so a flapping app goes quiet.
	r.Clear("app-1", KindAppCrashed)
	if len(mail.sent) != 4 {
		t.Fatalf("sent %d messages, want 4 (the second recovery)", len(mail.sent))
	}
	r.Record(ev(KindAppCrashed, "rel-1"))
	if len(mail.sent) != 4 {
		t.Fatalf("sent %d messages, want still 4: the third failure in one window is silent", len(mail.sent))
	}
}

// Nothing about a recorder is allowed to panic on a caller that has none, and
// nothing is allowed to fail because there is no relay.
func TestNilRecorderAndNilMailerAreInert(t *testing.T) {
	var nilRec *Recorder
	nilRec.Record(ev(KindAppCrashed, "rel-1"))
	nilRec.Clear("app-1", KindAppCrashed)
	nilRec.SetCooldown(time.Second)

	store := newFakeStore()
	r := NewRecorder(store, nil, nil)
	r.Record(ev(KindDeployFailed, "build"))
	r.Clear("app-1", KindDeployFailed)
	if len(store.events) != 1 {
		t.Fatalf("with no mailer, recorded %d rows, want 1", len(store.events))
	}

	// A recorder with no store does nothing rather than panicking.
	NewRecorder(nil, nil, nil).Record(ev(KindDeployFailed, "build"))
}

// An event with no app or no kind is a bug at the call site; drop it rather
// than writing a row nothing can filter.
func TestRecordRejectsIncompleteEvents(t *testing.T) {
	r, store, _ := newRecorder(t)
	r.Record(Event{Kind: KindAppCrashed})
	r.Record(Event{AppID: "app-1"})
	if len(store.events) != 0 {
		t.Fatalf("recorded %d incomplete events, want 0", len(store.events))
	}
}

// A failure to write the row stops the send: mailing about something that is
// not on the record would leave a reader with nowhere to look.
func TestInsertFailureSendsNothing(t *testing.T) {
	r, store, mail := newRecorder(t)
	store.insertErr = errors.New("database is down")
	r.Record(ev(KindAppCrashed, "rel-1"))
	if len(mail.sent) != 0 {
		t.Fatalf("sent %d messages after the row failed to write, want 0", len(mail.sent))
	}
}

// Deleting an app races its own async writers, and this is the half that used
// to shout about it.
//
// Recording is out of the request path on purpose — *telling someone never
// fails the thing that broke* — so a watcher observing a crash and an
// administrator pressing Delete are always able to cross. Logging that at error
// makes the level stop meaning what it says, which the e2e log scan noticed by
// failing a case for an unexpected error-level line. The signal that matters —
// the platform could not write down what it saw — has to survive, so both
// directions are asserted here.
func TestADeletedAppsEventIsNotAnErrorLevelLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want slog.Level
	}{
		{"the app was deleted underneath the writer", database.ErrAppGone, slog.LevelDebug},
		{"the write genuinely failed", errors.New("database is down"), slog.LevelError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			restore := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			defer slog.SetDefault(restore)

			r, store, mail := newRecorder(t)
			store.insertErr = tc.err
			r.Record(ev(KindAppCrashed, "rel-1"))

			if !strings.Contains(buf.String(), "level="+tc.want.String()) {
				t.Errorf("want a %s line, got: %s", tc.want, buf.String())
			}
			if tc.want == slog.LevelDebug && strings.Contains(buf.String(), "level=ERROR") {
				t.Errorf("an expected race was still logged at error: %s", buf.String())
			}
			// Either way nothing is sent: mailing about something that is not
			// on the record leaves a reader with nowhere to look.
			if len(mail.sent) != 0 {
				t.Errorf("sent %d messages after the row failed to write", len(mail.sent))
			}
		})
	}
}

// An app whose people have no addresses is a warning, not a crash.
func TestNoRecipientsIsHarmless(t *testing.T) {
	r, store, mail := newRecorder(t)
	store.people = nil
	r.Record(ev(KindAppCrashed, "rel-1"))
	if len(mail.sent) != 0 {
		t.Fatalf("sent %d messages with no recipients", len(mail.sent))
	}
	if len(store.events) != 1 {
		t.Fatal("the event was not recorded")
	}
}

// The mail says what happened, points at the app, and carries the detail.
func TestNotificationContent(t *testing.T) {
	r, _, mail := newRecorder(t)
	e := ev(KindDeployFailed, "build")
	e.Detail = "npm ERR! missing script: build"
	r.Record(e)

	if len(mail.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(mail.sent))
	}
	msg := mail.sent[0]
	if !strings.Contains(msg.Subject, "Quarterly Report") {
		t.Errorf("subject %q does not name the app", msg.Subject)
	}
	if !strings.Contains(msg.Subject, "token") {
		t.Errorf("subject %q does not carry the summary", msg.Subject)
	}
	if !strings.Contains(msg.Text, "npm ERR!") {
		t.Error("body does not carry the detail")
	}
	// The app's **id**. The slug this used to assert is the apps origin's
	// identifier and addresses the running app, not its page here — see
	// notify.AppPageURL.
	if !strings.Contains(msg.Text, "https://toft.internal/apps/app-1") {
		t.Errorf("body does not link to the app's page: %s", msg.Text)
	}
	if strings.Contains(msg.Text, "/apps/reports") {
		t.Errorf("body links by slug, which addresses the running app: %s", msg.Text)
	}
	if msg.Audience != mailer.AudienceApp {
		t.Errorf("audience = %q, want %q — otherwise the admin switch silences the wrong half",
			msg.Audience, mailer.AudienceApp)
	}
	if msg.To[0] != "ada@example.com" {
		t.Errorf("recipients = %v", msg.To)
	}
}

// A build log is bounded before it goes anywhere near an email body.
func TestDetailIsTrimmed(t *testing.T) {
	r, store, _ := newRecorder(t)
	e := ev(KindDeployFailed, "build")
	e.Detail = strings.Repeat("a line of build output\n", 2000)
	r.Record(e)

	got := store.events[0].Detail
	if len(got) > DetailLimit+64 {
		t.Fatalf("stored detail is %d bytes, want at most ~%d", len(got), DetailLimit)
	}
	if !strings.Contains(got, "trimmed") {
		t.Error("a truncated excerpt does not say it was truncated")
	}
}

func TestTailKeepsTheEnd(t *testing.T) {
	if got := tail("short", 100); got != "short" {
		t.Fatalf("tail did not pass short input through: %q", got)
	}
	got := tail("aaaa\nbbbb\ncccc\ndddd", 10)
	if !strings.HasSuffix(got, "dddd") {
		t.Fatalf("tail kept the wrong end: %q", got)
	}
	if strings.Contains(got, "aaaa") {
		t.Fatalf("tail kept trimmed content: %q", got)
	}
}

// A failure whose output holds binary is the one most worth recording, and a
// NUL or a cut through a rune would make Postgres refuse the row (JOBS-2).
func TestEventTextIsValidForPostgres(t *testing.T) {
	r, store, _ := newRecorder(t)
	e := ev(KindDeployFailed, "build")
	e.Summary = "bad \x00 summary"
	// No newline, so the cut lands wherever DetailLimit says, and every rune
	// is three bytes so most cuts land inside one.
	e.Detail = "\x00" + strings.Repeat("€", DetailLimit)
	r.Record(e)

	got := store.events[0]
	for name, s := range map[string]string{"summary": got.Summary, "detail": got.Detail} {
		if !utf8.ValidString(s) || strings.IndexByte(s, 0) >= 0 {
			t.Errorf("%s is not valid for a text column: %q", name, s[:min(len(s), 40)])
		}
	}
	if short := tail("a\x00b", 100); short != "a�b" {
		t.Errorf("tail of short input keeps the NUL: %q", short)
	}
}

// One automation's success resolves its own failure and nobody else's. Closing
// on kind alone told an owner their failing nightly job had recovered the moment
// an hourly one succeeded — the wrong mail, with nothing wrong-looking about it.
func TestOneJobsSuccessDoesNotClearAnothersFailure(t *testing.T) {
	for _, kind := range []Kind{KindJobRunFailed, KindJobRunOverdue} {
		t.Run(string(kind), func(t *testing.T) {
			r, store, mail := newRecorder(t)
			r.Record(ev(kind, "nightly-id:app_exec"))
			if len(mail.sent) != 1 {
				t.Fatalf("setup: expected the failure mail, got %d", len(mail.sent))
			}

			r.ClearAutomation("app-1", kind, "hourly-id", "hourly")

			if len(mail.sent) != 1 {
				t.Errorf("another automation's success sent a recovery (%d messages)", len(mail.sent))
			}
			if open := openOf(store, kind); open != 1 {
				t.Fatalf("open %s events = %d, want the nightly failure still open", kind, open)
			}

			r.ClearAutomation("app-1", kind, "nightly-id", "nightly")

			if openOf(store, kind) != 0 {
				t.Error("the automation's own success left its failure open")
			}
			if len(mail.sent) != 2 || !strings.Contains(mail.sent[1].Subject+mail.sent[1].Text, "nightly") {
				t.Errorf("recovery = %+v, want one notice naming the automation", mail.sent[len(mail.sent)-1])
			}
		})
	}
}

func openOf(store *fakeStore, kind Kind) int {
	n := 0
	for _, e := range store.events {
		if e.Kind == string(kind) && e.ResolvedAt == nil {
			n++
		}
	}
	return n
}

// VN-5 (2026-11-15): a summary can carry text a job's own code reported, so it
// is reduced to one bounded line before it becomes the Events row and the mail
// subject.
func TestALongMultiLineSummaryMailsOneBoundedLine(t *testing.T) {
	r, store, mail := newRecorder(t)

	e := ev(KindJobRunFailed, "def:app_exec")
	e.Summary = "first line\n" + strings.Repeat("z", 5000)
	r.Record(e)
	e2 := ev(KindJobRunFailed, "def2:app_exec")
	e2.Summary = strings.Repeat("w", 5000)
	r.Record(e2)

	if len(mail.sent) != 2 {
		t.Fatalf("sent %d messages, want 2", len(mail.sent))
	}
	for _, m := range mail.sent {
		if strings.ContainsAny(m.Subject, "\r\n") || len(m.Subject) > SummaryLimit+200 {
			t.Errorf("subject is %d bytes or multi-line: %.80q", len(m.Subject), m.Subject)
		}
	}
	for _, row := range store.events {
		if strings.ContainsAny(row.Summary, "\r\n") || len(row.Summary) > SummaryLimit {
			t.Errorf("stored summary is %d bytes or multi-line", len(row.Summary))
		}
	}
	if store.events[0].Summary != "first line" {
		t.Errorf("stored summary = %q, want its first line", store.events[0].Summary)
	}
}

// A dependency refusal's recovery says what ended, not the kind's name.
func TestDependencyBlockedHasARecoverySentence(t *testing.T) {
	got := recoverySummary(KindDependencyBlocked)
	if strings.Contains(got, "has cleared") {
		t.Fatalf("recovery for a dependency refusal fell to the default: %q", got)
	}
}
