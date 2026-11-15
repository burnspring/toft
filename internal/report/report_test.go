package report

import (
	"context"
	"errors"
	"testing"
	"time"

	"toft/internal/auth"
	"toft/internal/database"
	"toft/internal/mail"
)

// The delivery rules, and the first three lead because their failure would be
// **silent**: mail that keeps arriving, or stops arriving, with nothing anywhere
// saying so and nobody who would think to look here.

type fakeStore struct {
	schedule   *database.RenderSchedule
	recipients []database.ScheduleRecipient
	users      map[string]*database.User
	roles      map[string]string // userID -> object role, "" for none
	last       *time.Time
	settings   database.PlatformSettings

	recorded []*database.ReportDelivery
}

func (f *fakeStore) GetRenderSchedule(string) (*database.RenderSchedule, error) {
	return f.schedule, nil
}
func (f *fakeStore) ListScheduleRecipients(string) ([]database.ScheduleRecipient, error) {
	return f.recipients, nil
}
func (f *fakeStore) LastReportDeliveryAt(string) (*time.Time, error) { return f.last, nil }
func (f *fakeStore) RecordReportDelivery(_ context.Context, d *database.ReportDelivery) error {
	for _, r := range d.Recipients {
		switch r.Status {
		case database.DeliveryDelivered:
			d.Delivered++
		case database.DeliverySkipped:
			d.Skipped++
		case database.DeliveryFailed:
			d.Failed++
		}
	}
	f.recorded = append(f.recorded, d)
	return nil
}
func (f *fakeStore) GetUser(id string) (*database.User, error) { return f.users[id], nil }
func (f *fakeStore) PrincipalForUser(id string) (*auth.Principal, error) {
	u := f.users[id]
	if u == nil {
		return nil, nil
	}
	return &auth.Principal{Type: database.PrincipalTypeUser, ID: u.ID, SystemRole: u.SystemRole}, nil
}
func (f *fakeStore) GetPlatformSettings() (database.PlatformSettings, error) {
	return f.settings, nil
}

// EffectiveRole is the RoleResolver the authorizer consults. The fake answers
// from a map so a test can revoke access without building a grant graph.
func (f *fakeStore) EffectiveRole(objectID, principalType, principalID string, groups []string) (string, error) {
	return f.roles[principalID], nil
}

type fakeMailer struct {
	sent []mail.Message
	err  error
}

func (m *fakeMailer) SendOne(_ context.Context, msg mail.Message) error {
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, msg)
	return nil
}

func fixture(t *testing.T) (*Deliverer, *fakeStore, *fakeMailer, *database.App) {
	t.Helper()
	store := &fakeStore{
		schedule: &database.RenderSchedule{ID: "sch-1", AppID: "app-1", Schedule: "0 8 * * 1", Enabled: true},
		users: map[string]*database.User{
			"owner":  {ID: "owner", Login: "olive", Email: "olive@example.com", SystemRole: database.SystemRolePublisher},
			"reader": {ID: "reader", Login: "ana", Email: "ana@example.com", SystemRole: database.SystemRoleViewer},
		},
		roles:    map[string]string{"reader": database.ObjectRoleViewer},
		settings: database.PlatformSettings{ReportDeliveryMinIntervalMinutes: 15},
	}
	store.recipients = []database.ScheduleRecipient{
		{UserID: "owner", Login: "olive", Email: "olive@example.com"},
		{UserID: "reader", Login: "ana", Email: "ana@example.com"},
	}
	app := &database.App{
		ID: "app-1", Slug: "weekly", Name: "Weekly report",
		OwnerID: "owner", Visibility: database.VisibilityPrivate,
	}
	mailer := &fakeMailer{}
	return New(store, mailer, store), store, mailer, app
}

func statusOf(d *database.ReportDelivery, login string) (string, string) {
	for _, r := range d.Recipients {
		if r.Login == login {
			return r.Status, r.Reason
		}
	}
	return "", ""
}

// SILENT #1. A revocation that does not reach the mail is a report link arriving
// weekly at somebody the install has decided cannot see the app. Nothing errors,
// the mail looks fine, and the only person who could notice is the one receiving
// something they should not.
func TestARecipientWhoLostAccessIsSkippedRatherThanMailed(t *testing.T) {
	d, store, mailer, app := fixture(t)
	delete(store.roles, "reader") // the grant is withdrawn

	d.Deliver(context.Background(), app, "dep-1", "https://apps.example.com/apps/weekly")

	if len(store.recorded) != 1 {
		t.Fatalf("recorded %d deliveries, want 1", len(store.recorded))
	}
	status, reason := statusOf(store.recorded[0], "ana")
	if status != database.DeliverySkipped {
		t.Errorf("ana = %q, want skipped — she can no longer open the app", status)
	}
	if reason == "" {
		t.Error("a skip with no reason is an absence, not a record")
	}
	for _, m := range mailer.sent {
		for _, to := range m.To {
			if to == "ana@example.com" {
				t.Error("a report link was mailed to somebody whose access was removed")
			}
		}
	}
	// And she is still on the list: the list is the owner's intent, and a
	// week's outage may not rewrite who the report is for.
	if len(store.recipients) != 2 {
		t.Errorf("the recipient list was rewritten by a send: %+v", store.recipients)
	}
}

// SILENT #2. `EffectiveRole` reads grants alone, so the app's own owner has no
// role on it. Filtering a recipient list through that function skips the person
// who built the report — and it looks like they mistyped their own name into
// the picker.
func TestTheAppsOwnerIsAValidRecipient(t *testing.T) {
	d, store, mailer, app := fixture(t)
	// Deliberately no grant for the owner, which is the ordinary state: an
	// owner owns the app, they are not granted it.
	if _, ok := store.roles["owner"]; ok {
		t.Fatal("the fixture gave the owner a grant, which is not the case under test")
	}

	d.Deliver(context.Background(), app, "dep-1", "https://apps.example.com/apps/weekly")

	status, reason := statusOf(store.recorded[0], "olive")
	if status != database.DeliveryDelivered {
		t.Fatalf("the owner = %q (%s), want delivered — EffectiveRole answers \"\" for an owner, "+
			"so this is what a grants-only check would break", status, reason)
	}
	if len(mailer.sent) == 0 {
		t.Fatal("nothing was sent")
	}
}

// SILENT #3. A lock is enforced at sign-in and at the door, never in the
// authorizer — an auth.Principal has no field for it, because one only exists
// for somebody who already got in. A report builds a principal for somebody who
// is *not* here, so without a separate check a deactivated account keeps
// receiving the Monday report: the offboarding hole SCIM closes on the
// authentication side, reopened on the mail side.
func TestALockedAccountIsNotMailed(t *testing.T) {
	d, store, mailer, app := fixture(t)
	locked := time.Now().UTC()
	store.users["reader"].LockedAt = &locked

	d.Deliver(context.Background(), app, "dep-1", "https://apps.example.com/apps/weekly")

	status, reason := statusOf(store.recorded[0], "ana")
	if status != database.DeliverySkipped {
		t.Errorf("a locked account = %q, want skipped", status)
	}
	if reason != "this account is locked" {
		t.Errorf("reason = %q, want the lock named — a locked account and a removed grant "+
			"are different facts", reason)
	}
	for _, m := range mailer.sent {
		for _, to := range m.To {
			if to == "ana@example.com" {
				t.Error("a locked account was mailed")
			}
		}
	}
}

// The floor exists because a cron accepts `* * * * *`. A suppressed run is a
// recorded skip rather than a silence, so the record can say why Monday's second
// delivery did not arrive.
func TestTwoRendersInsideTheFloorProduceOneDelivery(t *testing.T) {
	d, store, mailer, app := fixture(t)
	recent := time.Now().UTC().Add(-2 * time.Minute)
	store.last = &recent

	d.Deliver(context.Background(), app, "dep-2", "https://apps.example.com/apps/weekly")

	if len(mailer.sent) != 0 {
		t.Errorf("sent %d messages inside the floor, want none", len(mailer.sent))
	}
	if len(store.recorded) != 1 {
		t.Fatalf("a suppressed run wrote %d rows, want 1 — silence is not an answer", len(store.recorded))
	}
	for _, r := range store.recorded[0].Recipients {
		if r.Status != database.DeliverySkipped || r.Reason == "" {
			t.Errorf("%s = %q (%q), want a skip with a reason", r.Login, r.Status, r.Reason)
		}
	}
}

// A floor of zero is an install that has decided its relay can take it, stated
// rather than smuggled in as a very small number.
func TestAFloorOfZeroSuppressesNothing(t *testing.T) {
	d, store, mailer, app := fixture(t)
	store.settings.ReportDeliveryMinIntervalMinutes = 0
	recent := time.Now().UTC().Add(-1 * time.Second)
	store.last = &recent

	d.Deliver(context.Background(), app, "dep-2", "https://apps.example.com/apps/weekly")

	if len(mailer.sent) == 0 {
		t.Error("a zero floor suppressed a send")
	}
}

// An empty list is not an error and not a delivery: the schedule is a re-render
// exactly as it was before anybody attached a list.
func TestAnEmptyListSendsNothingAndRecordsNothing(t *testing.T) {
	d, store, mailer, app := fixture(t)
	store.recipients = nil

	d.Deliver(context.Background(), app, "dep-1", "https://apps.example.com/apps/weekly")

	if len(mailer.sent) != 0 || len(store.recorded) != 0 {
		t.Errorf("an empty list sent %d and recorded %d, want none of either",
			len(mailer.sent), len(store.recorded))
	}
}

// One recipient failing does not stop the rest. A rate limit that refuses the
// fortieth address is not a reason the first thirty-nine should not have gone.
func TestARelayFailureIsPerRecipient(t *testing.T) {
	d, store, mailer, app := fixture(t)
	mailer.err = errors.New("451 4.7.1 rate limited")

	d.Deliver(context.Background(), app, "dep-1", "https://apps.example.com/apps/weekly")

	got := store.recorded[0]
	if got.Failed != 2 {
		t.Errorf("failed = %d, want both recipients", got.Failed)
	}
	for _, r := range got.Recipients {
		if r.Reason == "" {
			t.Errorf("%s failed with no reason — the relay's own words are the record", r.Login)
		}
	}
}

// No relay is not an error and not a silence: every recipient is skipped with
// that reason, so an administrator who never configured mail can see why the
// report they set up is not arriving.
func TestNoRelayIsARecordedSkip(t *testing.T) {
	store := &fakeStore{
		schedule:   &database.RenderSchedule{ID: "sch-1", AppID: "app-1"},
		recipients: []database.ScheduleRecipient{{UserID: "owner", Login: "olive", Email: "olive@example.com"}},
		users:      map[string]*database.User{"owner": {ID: "owner", Login: "olive", Email: "olive@example.com"}},
		roles:      map[string]string{},
		settings:   database.PlatformSettings{},
	}
	app := &database.App{ID: "app-1", Slug: "weekly", Name: "Weekly", OwnerID: "owner"}
	d := New(store, nil, store)

	d.Deliver(context.Background(), app, "dep-1", "https://apps.example.com/apps/weekly")

	if len(store.recorded) != 1 {
		t.Fatalf("recorded %d, want 1", len(store.recorded))
	}
	_, reason := statusOf(store.recorded[0], "olive")
	if reason == "" {
		t.Error("no relay produced a skip with no reason")
	}
}

// A recipient with no address is named back rather than logged, for the reason
// the share notice already names it back: the directory picker carries no
// address, so this is discoverable nowhere else.
func TestARecipientWithNoAddressIsNamedBack(t *testing.T) {
	d, store, _, app := fixture(t)
	store.users["reader"].Email = ""
	store.recipients[1].Email = ""

	d.Deliver(context.Background(), app, "dep-1", "https://apps.example.com/apps/weekly")

	status, reason := statusOf(store.recorded[0], "ana")
	if status != database.DeliverySkipped || reason != "no address on file" {
		t.Errorf("ana = %q (%q), want a skip naming the missing address", status, reason)
	}
}

// The link in the mail is the app's address, passed in already built. Every
// other link in this product's mail points at the platform; on a split-origin
// install those are different hosts.
func TestTheLinkIsTheAppsAddress(t *testing.T) {
	d, _, mailer, app := fixture(t)
	const appURL = "https://apps.example.com/apps/weekly"

	d.Deliver(context.Background(), app, "dep-1", appURL)

	if len(mailer.sent) == 0 {
		t.Fatal("nothing was sent")
	}
	for _, m := range mailer.sent {
		if !contains(m.Text, appURL) {
			t.Errorf("the message does not carry the app's address:\n%s", m.Text)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// blockingMailer sends nothing until its context ends — a relay that accepts
// and stalls.
type blockingMailer struct{}

func (blockingMailer) SendOne(ctx context.Context, _ mail.Message) error {
	<-ctx.Done()
	return ctx.Err()
}

// deadlineStore refuses a record whose context has already ended, as the real
// database would.
type deadlineStore struct{ *fakeStore }

func (s deadlineStore) RecordReportDelivery(ctx context.Context, d *database.ReportDelivery) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.fakeStore.RecordReportDelivery(ctx, d)
}

// One run is bounded by its caller's deadline, not 30 s a recipient, and the
// row saying every send failed still lands after it (security review
// 2026-11-14, PB-8).
func TestDeliverUnderADeadlineReturnsAndRecordsEveryFailure(t *testing.T) {
	_, store, _, app := fixture(t)
	store.users["third"] = &database.User{ID: "third", Login: "tom", Email: "tom@example.com", SystemRole: database.SystemRoleViewer}
	store.roles["third"] = database.ObjectRoleViewer
	store.recipients = append(store.recipients, database.ScheduleRecipient{UserID: "third", Login: "tom", Email: "tom@example.com"})
	ds := deadlineStore{store}
	d := New(ds, blockingMailer{}, store)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	d.Deliver(ctx, app, "dep-1", "https://apps.example/weekly/")
	if took := time.Since(start); took > time.Second {
		t.Fatalf("Deliver took %s under a 200ms deadline", took)
	}
	if len(store.recorded) != 1 {
		t.Fatalf("recorded %d deliveries, want 1", len(store.recorded))
	}
	for _, login := range []string{"olive", "ana", "tom"} {
		if status, _ := statusOf(store.recorded[0], login); status != database.DeliveryFailed {
			t.Errorf("%s: status %q, want failed", login, status)
		}
	}
}
