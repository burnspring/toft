package appint

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"toft/internal/database"
)

// fakeStore is the four gates' inputs, and nothing else. No Postgres: the whole
// point of declaring Store here is that the decision this package makes is
// testable without one.
type fakeStore struct {
	// attach is the one input the gates have, by kind — which is the change the
	// catalog made: there is no longer a settings document plus a table per
	// integration to disagree with each other.
	attach map[string]database.Attachment
	// app is what GetApp answers; nil is an app that is not locked.
	app *database.App

	mu        sync.Mutex
	inserted  []*database.IntegrationSend
	completed []completion
	byKey     map[string]*database.IntegrationSend
}

type completion struct{ id, status, err string }

func (f *fakeStore) ResolveAttachment(_, kind string) (database.Attachment, error) {
	return f.attach[kind], nil
}

func (f *fakeStore) GetApp(string) (*database.App, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.app, nil
}

func (f *fakeStore) InsertSend(s *database.IntegrationSend) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inserted = append(f.inserted, s)
	return nil
}

func (f *fakeStore) CompleteSend(id, status, errMsg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed = append(f.completed, completion{id, status, errMsg})
	return nil
}

func (f *fakeStore) FindSendByIdempotencyKey(_, key string, _ time.Duration) (*database.IntegrationSend, error) {
	return f.byKey[key], nil
}

func (f *fakeStore) outcomes() []completion {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]completion(nil), f.completed...)
}

// bothOn is an install where an administrator has switched both integrations on
// and attached both to this app — the state every gate test varies one field
// of. There is nothing further to switch on: posting a file is gated as posting
// is, and attaching as sending is (migration 44).
func bothOn() *fakeStore {
	on := database.Attachment{Connected: true, Attached: true, Enabled: true}
	return &fakeStore{attach: map[string]database.Attachment{
		IntegrationSlack: on,
		IntegrationEmail: on,
	}}
}

// off rewrites one kind's attachment, for the table below.
func off(kind string, mutate func(*database.Attachment)) func(*fakeStore) {
	return func(f *fakeStore) {
		a := f.attach[kind]
		mutate(&a)
		f.attach[kind] = a
	}
}

func TestEachGateRefusesOnItsOwn(t *testing.T) {
	cases := []struct {
		name       string
		capability string
		mutate     func(*fakeStore)
		status     string
		wantCode   string
	}{
		{"nothing connected", CapSlackPost,
			off(IntegrationSlack, func(a *database.Attachment) { a.Connected = false }),
			"", "slack_off"},
		{"not attached", CapSlackPost,
			off(IntegrationSlack, func(a *database.Attachment) { a.Attached = false }),
			"", "slack_not_attached"},
		{"attached but switched off", CapSlackPost,
			off(IntegrationSlack, func(a *database.Attachment) { a.Enabled = false }),
			"", "slack_disabled"},
		{"file upload follows the Slack switch", CapSlackFile,
			off(IntegrationSlack, func(a *database.Attachment) { a.Enabled = false }),
			"", "slack_disabled"},
		{"email not connected", CapEmailSend,
			off(IntegrationEmail, func(a *database.Attachment) { a.Connected = false }),
			"", "email_off"},
		{"email not attached", CapEmailSend,
			off(IntegrationEmail, func(a *database.Attachment) { a.Attached = false }),
			"", "email_not_attached"},
		{"attaching follows the email switch", CapEmailAttach,
			off(IntegrationEmail, func(a *database.Attachment) { a.Enabled = false }),
			"", "email_disabled"},
		{"stopped app", CapSlackPost,
			func(*fakeStore) {}, database.AppStatusStopped, "app_stopped"},
		{"capability nobody offers", "carrier.pigeon",
			func(*fakeStore) {}, "", "unknown_capability"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := bothOn()
			tc.mutate(store)
			d, err := Check(store, "app1", tc.status, tc.capability)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if d.Allowed {
				t.Fatalf("allowed, want refusal %q", tc.wantCode)
			}
			if d.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", d.Code, tc.wantCode)
			}
			// The standing rule on this surface: a refusal names the remedy, so
			// a developer reading a log knows who to ask and for what.
			if d.Reason == "" {
				t.Error("refusal carries no reason")
			}
			if d.Status == 0 {
				t.Error("refusal carries no HTTP status")
			}
			// And the remedy is in the hint, the field an agent acts on —
			// every gate's, an unknown capability's included (BB-042, fourth
			// 2026-11-03 black-box list).
			if d.Hint == "" {
				t.Errorf("hint = %q for %s", d.Hint, tc.wantCode)
			}
		})
	}
}

// A slept app is not a stopped one (ADR §16): a request can wake it, and the
// very next thing it does may be to send.
func TestSleepingAppMaySend(t *testing.T) {
	for _, status := range []string{"", database.AppStatusRunning, database.AppStatusSleeping} {
		d, err := Check(bothOn(), "app1", status, CapSlackPost)
		if err != nil {
			t.Fatalf("Check(%q): %v", status, err)
		}
		if !d.Allowed {
			t.Errorf("status %q was refused: %s", status, d.Reason)
		}
	}
}

// A stopped app is refused even where every capability is attached — and the
// refusal names the state rather than the capability, because the state is what
// the caller can fix.
func TestStoppedAppIsRefusedWithItsOwnStatus(t *testing.T) {
	d, _ := Check(bothOn(), "app1", database.AppStatusStopped, CapSlackPost)
	if d.Allowed {
		t.Fatal("a stopped app was allowed to send")
	}
	if d.Status != 409 {
		t.Errorf("status = %d, want 409 — a stopped app is a conflict, not a permission problem", d.Status)
	}
}

// Attaching an integration grants both of its actions. There is no second
// switch, so an app that may post may post a file and an app that may send may
// attach one — the property the per-action gates were removed to get.
func TestAttachingGrantsBothActions(t *testing.T) {
	for _, capability := range []string{CapSlackPost, CapSlackFile, CapEmailSend, CapEmailAttach} {
		d, err := Check(bothOn(), "app1", database.AppStatusRunning, capability)
		if err != nil {
			t.Fatalf("Check(%q): %v", capability, err)
		}
		if !d.Allowed {
			t.Errorf("%q was refused on an attached app: %s", capability, d.Reason)
		}
	}
}

func TestCapabilitiesReportWhatIsAttached(t *testing.T) {
	caps, err := Capabilities(bothOn(), "app1")
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	got := strings.Join(caps, ",")
	for _, want := range []string{CapSlackPost, CapSlackFile, CapEmailSend, CapEmailAttach} {
		if !strings.Contains(got, want) {
			t.Errorf("capabilities %v missing %q", caps, want)
		}
	}
}

// Switching an integration off takes its file action with it — the other half
// of the same property, and the one an app discovers through toft.app.can().
func TestSwitchingOffTakesBothActions(t *testing.T) {
	store := bothOn()
	off(IntegrationSlack, func(a *database.Attachment) { a.Enabled = false })(store)
	off(IntegrationEmail, func(a *database.Attachment) { a.Attached = false })(store)

	caps, err := Capabilities(store, "app1")
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if len(caps) != 0 {
		t.Errorf("capabilities = %v, want none", caps)
	}
}

// An install that has not switched an integration on reports nothing at all for
// it — absent, not "attached but unavailable".
func TestCapabilitiesAreEmptyWhenTheInstallOffersNothing(t *testing.T) {
	store := bothOn()
	store.attach = nil
	caps, err := Capabilities(store, "app1")
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if len(caps) != 0 {
		t.Errorf("capabilities = %v, want none", caps)
	}
}

// An attachment whose connection has not switched tokens on lists no token
// capability, and one whose connection has lists it. A failure here is every
// attachment silently widened on upgrade.
func TestTheTokenIsListedOnlyWhileTheConnectionIssuesThem(t *testing.T) {
	on := database.Attachment{Connected: true, Attached: true, Enabled: true}
	store := &fakeStore{attach: map[string]database.Attachment{IntegrationSharePoint: on}}
	caps, err := Capabilities(store, "app1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(caps, ",") != "sharepoint.upload" {
		t.Errorf("switch off: capabilities = %v, want only the upload", caps)
	}

	on.IssueTokens = true
	store.attach[IntegrationSharePoint] = on
	caps, _ = Capabilities(store, "app1")
	if strings.Join(caps, ",") != "sharepoint.upload,sharepoint.token" {
		t.Errorf("switch on: capabilities = %v", caps)
	}
}

func TestIntegrationAndActionSplit(t *testing.T) {
	if got := IntegrationOf(CapEmailSend); got != IntegrationEmail {
		t.Errorf("IntegrationOf = %q, want %q", got, IntegrationEmail)
	}
	if got := ActionOf(CapEmailSend); got != "send" {
		t.Errorf("ActionOf = %q, want %q", got, "send")
	}
	if got := IntegrationOf("slack"); got != "slack" {
		t.Errorf("IntegrationOf on a bare name = %q, want %q", got, "slack")
	}
}

// --------------------------------------------------------------------- queue

func job(store *fakeStore, id string) Job {
	send := &database.IntegrationSend{
		ID: id, AppID: "app1", Kind: IntegrationSlack,
		Action: "post", Status: database.SendQueued,
	}
	_ = store.InsertSend(send)
	return Job{Send: send, Capability: CapSlackPost, Payload: Payload{Text: "hi"}}
}

func drain(t *testing.T, s *Sender) {
	t.Helper()
	s.Start()
	s.Stop()
}

// Off means *now*, not merely "for the next one". A capability withdrawn
// between acceptance and delivery stops the send that is already in flight.
func TestAGateClosingAfterAcceptanceBlocksTheSend(t *testing.T) {
	store := bothOn()
	delivered := false
	sender := NewSender(store, func(context.Context, Job) error {
		delivered = true
		return nil
	})
	if err := sender.Enqueue(job(store, "s1")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// The administrator detaches while it sits in the queue.
	off(IntegrationSlack, func(a *database.Attachment) { a.Attached = false })(store)
	drain(t, sender)

	if delivered {
		t.Error("the message was delivered after its integration was detached")
	}
	out := store.outcomes()
	if len(out) != 1 || out[0].status != database.SendBlocked {
		t.Fatalf("outcomes = %+v, want one blocked", out)
	}
	// blocked, not failed: nothing was wrong with the message, and an owner
	// reading the ledger needs to see which of the two happened.
	if out[0].err == "" {
		t.Error("a blocked send does not say why")
	}
}

// A send queued before its app was locked is not delivered after the lock: the
// worker re-reads the lock, though not the status (security review
// 2026-11-15, LSA-3).
func TestALockAfterAcceptanceBlocksTheSend(t *testing.T) {
	store := bothOn()
	delivered := false
	sender := NewSender(store, func(context.Context, Job) error {
		delivered = true
		return nil
	})
	if err := sender.Enqueue(job(store, "s1")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	now := time.Now()
	store.mu.Lock()
	store.app = &database.App{LockedAt: &now}
	store.mu.Unlock()
	drain(t, sender)

	if delivered {
		t.Error("the message was delivered after its app was locked")
	}
	out := store.outcomes()
	if len(out) != 1 || out[0].status != database.SendBlocked {
		t.Fatalf("outcomes = %+v, want one blocked", out)
	}
	if out[0].err == "" {
		t.Error("a blocked send does not say why")
	}
}

func TestASuccessfulSendIsRecordedSent(t *testing.T) {
	store := bothOn()
	sender := NewSender(store, func(context.Context, Job) error { return nil })
	_ = sender.Enqueue(job(store, "s1"))
	drain(t, sender)

	out := store.outcomes()
	if len(out) != 1 || out[0].status != database.SendSent {
		t.Fatalf("outcomes = %+v, want one sent", out)
	}
}

// One retry, and only for a failure that might not repeat. A report that
// arrives twice is worse than one that arrives once and is recorded as failed.
func TestARetryableFailureIsAttemptedExactlyTwice(t *testing.T) {
	store := bothOn()
	var mu sync.Mutex
	attempts := 0
	sender := NewSender(store, func(context.Context, Job) error {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		return Retryable(errors.New("slack is having an afternoon"))
	})
	_ = sender.Enqueue(job(store, "s1"))
	sender.Start()
	// Stop cancels the retry wait, so give the one retry room to happen.
	time.Sleep(RetryDelay + 500*time.Millisecond)
	sender.Stop()

	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != 2 {
		t.Errorf("attempts = %d, want 2 (one try, one retry)", got)
	}
}

func TestANonRetryableFailureIsAttemptedOnce(t *testing.T) {
	store := bothOn()
	var mu sync.Mutex
	attempts := 0
	sender := NewSender(store, func(context.Context, Job) error {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		return errors.New("channel_not_found")
	})
	_ = sender.Enqueue(job(store, "s1"))
	drain(t, sender)

	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != 1 {
		t.Errorf("attempts = %d, want 1 — a 4xx is still wrong the second time", got)
	}
	if out := store.outcomes(); len(out) != 1 || out[0].status != database.SendFailed {
		t.Fatalf("outcomes = %+v, want one failed", out)
	}
}

// A full queue refuses. It never accepts and discards: a 202 for a message that
// was thrown away is worse than a refusal, because the app stops watching.
func TestAFullQueueRefusesRatherThanDropping(t *testing.T) {
	store := bothOn()
	sender := NewSender(store, func(context.Context, Job) error { return nil })
	// Not started, so nothing drains it.
	// Spread over apps, so no one app reaches its own share first.
	for i := 0; i < QueueDepth; i++ {
		j := job(store, "s")
		j.Send.AppID = fmt.Sprintf("app%d", i%Workers)
		if err := sender.Enqueue(j); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}
	if err := sender.Enqueue(job(store, "overflow")); !errors.Is(err, ErrQueueFull) {
		t.Errorf("Enqueue past depth = %v, want ErrQueueFull", err)
	}
}

// One app's backlog is its own: it cannot take the slots every other app's send
// — a page included — needs.
func TestOneAppCannotFillTheQueue(t *testing.T) {
	store := bothOn()
	sender := NewSender(store, func(context.Context, Job) error { return nil })
	for i := 0; i < PerAppDepth; i++ {
		if err := sender.Enqueue(job(store, "a")); err != nil {
			t.Fatalf("Enqueue %d for app A: %v", i, err)
		}
	}
	err := sender.Enqueue(job(store, "a-over"))
	if !errors.Is(err, ErrQueueFull) || !strings.Contains(err.Error(), "this app") {
		t.Fatalf("app A past its share = %v, want ErrQueueFull naming its own backlog", err)
	}
	b := job(store, "b")
	b.Send.AppID = "app2"
	if err := sender.Enqueue(b); err != nil {
		t.Fatalf("app B was refused because of app A: %v", err)
	}
	drain(t, sender)
	if err := sender.Enqueue(job(store, "a-again")); err != nil {
		t.Errorf("app A after its sends finished: %v", err)
	}
}

func TestOneAppsBytesAreBounded(t *testing.T) {
	store := bothOn()
	sender := NewSender(store, func(context.Context, Job) error { return nil })
	big := func(id string) Job {
		j := job(store, id)
		j.Payload.Files = []File{{Name: "a.bin", Content: make([]byte, PerAppBytes/2+1)}}
		return j
	}
	// A single send larger than the share is still accepted when the app
	// holds nothing.
	huge := job(store, "huge")
	huge.Payload.Files = []File{{Name: "h.bin", Content: make([]byte, PerAppBytes+1)}}
	if err := sender.Enqueue(huge); err != nil {
		t.Fatalf("a lone oversize send was refused: %v", err)
	}
	drain(t, sender)

	sender = NewSender(store, func(context.Context, Job) error { return nil })
	if err := sender.Enqueue(big("b1")); err != nil {
		t.Fatalf("first large send: %v", err)
	}
	if err := sender.Enqueue(big("b2")); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("second large send = %v, want ErrQueueFull", err)
	}
	small := job(store, "small")
	if err := sender.Enqueue(small); err != nil {
		t.Errorf("a small send under the byte share was refused: %v", err)
	}
}

func TestOnFailureIsToldOnlyAboutFailures(t *testing.T) {
	store := bothOn()
	var mu sync.Mutex
	var told []string
	fail := true
	sender := NewSender(store, func(context.Context, Job) error {
		if fail {
			return errors.New("nope")
		}
		return nil
	})
	sender.OnFailure(func(j Job, cause string) {
		mu.Lock()
		defer mu.Unlock()
		told = append(told, j.Send.ID)
	})
	_ = sender.Enqueue(job(store, "bad"))
	sender.Start()
	time.Sleep(200 * time.Millisecond)
	fail = false
	_ = sender.Enqueue(job(store, "good"))
	sender.Stop()

	mu.Lock()
	defer mu.Unlock()
	if len(told) != 1 || told[0] != "bad" {
		t.Errorf("failure hook heard about %v, want only the failed send", told)
	}
}

func TestDigestIsStableAndSized(t *testing.T) {
	sum, size := Digest("subject", "body")
	again, _ := Digest("subject", "body")
	if sum != again {
		t.Error("the same content hashed to two different digests")
	}
	if size != len("subject")+len("body") {
		t.Errorf("size = %d, want %d", size, len("subject")+len("body"))
	}
	if other, _ := Digest("subject", "other"); other == sum {
		t.Error("different content hashed the same")
	}
	// The same bytes split at a different field boundary are a different
	// message, so they must not share the ledger's proof of identity.
	a, _ := Digest("Report", "A: done")
	b, _ := Digest("ReportA", ": done")
	if a == b {
		t.Error("moving the subject/body boundary left the digest unchanged")
	}
	if c, _ := Digest("Report", "A: done", ""); c == a {
		t.Error("an empty trailing part (no HTML) hashed the same as its absence")
	}
}

func TestMintedTokensAreDistinctAndPrefixed(t *testing.T) {
	a, hashA, prefixA, err := MintAppToken()
	if err != nil {
		t.Fatalf("MintAppToken: %v", err)
	}
	b, hashB, _, _ := MintAppToken()

	if !strings.HasPrefix(a, TokenPrefix) {
		t.Errorf("token %q does not start with %q", a, TokenPrefix)
	}
	// The three credential classes must stay distinguishable by prefix test.
	if strings.HasPrefix(a, "toft_") || strings.HasPrefix(a, "toftu_") {
		t.Errorf("token %q collides with another credential class", a)
	}
	if a == b || hashA == hashB {
		t.Error("two mints produced the same token")
	}
	if strings.Contains(hashA, a) || strings.Contains(a, hashA) {
		t.Error("the stored hash reveals the secret")
	}
	if prefixA != a[:8] {
		t.Errorf("prefix = %q, want %q", prefixA, a[:8])
	}
}

// The viewer-identity capabilities: one switch an administrator sets, and one
// derived fact the app is told rather than left to discover.
func TestViewerCapabilitiesAdvertiseTheEmailRule(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		enabled                         bool
		visibility, framework           string
		wantIdentity, wantIdentityEmail bool
	}{
		{"off is absent", false, "private", "streamlit", false, false},
		{"private carries the address", true, "private", "streamlit", true, true},
		{"authenticated carries the address", true, "authenticated", "flask", true, true},
		// A public app's visitors did not consent to a mailing list by visiting.
		{"public withholds the address", true, "public", "streamlit", true, false},
		// A static app runs no commands, so it has no process to hold a token
		// and no way to make the call. Absent, not failing at runtime.
		{"static gets neither", true, "private", "static", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caps := ViewerCapabilities(tc.enabled, tc.visibility, tc.framework)
			has := func(c string) bool {
				for _, got := range caps {
					if got == c {
						return true
					}
				}
				return false
			}
			if has(CapViewerIdentity) != tc.wantIdentity {
				t.Errorf("%s = %v, want %v", CapViewerIdentity, has(CapViewerIdentity), tc.wantIdentity)
			}
			if has(CapViewerIdentityEmail) != tc.wantIdentityEmail {
				t.Errorf("%s = %v, want %v", CapViewerIdentityEmail,
					has(CapViewerIdentityEmail), tc.wantIdentityEmail)
			}
		})
	}
}
