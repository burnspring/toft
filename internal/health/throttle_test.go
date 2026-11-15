package health

import (
	"strings"
	"testing"
	"time"

	"toft/internal/database"
	"toft/internal/events"
	"toft/internal/runtime"
)

// throttleStore is the watcher's store with an hour of throttling to read, and
// an event that is open while it has been recorded more often than cleared.
type throttleStore struct {
	*fakeStore
	rec  *recordingStore
	hour database.CPUThrottling
}

func (s *throttleStore) AppCPUThrottling(string, time.Time) (database.CPUThrottling, error) {
	return s.hour, nil
}

func (s *throttleStore) HasOpenAppEvent(_, kind string) (bool, error) {
	n := 0
	for _, e := range s.rec.events {
		if e.Kind == kind {
			n++
		}
	}
	return n > s.rec.cleared[kind], nil
}

// share is an hour of work with this fraction of its periods throttled.
func share(f float64, measured float64) database.CPUThrottling {
	return database.CPUThrottling{Periods: 36000, ThrottledPeriods: int64(36000 * f), MeasuredSeconds: measured}
}

func TestAnHourHeldAtTheCPULimitIsRaisedMailedOnceAndCleared(t *testing.T) {
	rec := &recordingStore{cleared: map[string]int{}}
	mail := &sentMail{}
	app := runningApp()
	served := time.Now().UTC().Add(-2 * time.Hour)
	app.ServingSince = &served
	store := &throttleStore{fakeStore: &fakeStore{apps: []database.App{app}, pending: map[string]bool{}},
		rec: rec, hour: share(0.26, 3600)}
	w := New(store, runtime.NewRuntimes(&stubRuntime{workloads: online(0)}, nil),
		events.NewRecorder(notifyingStore{rec}, mail), true, nil).
		WithCPULimit(func(*database.App) int { return 1000 })
	clock := time.Now().UTC()
	w.now = func() time.Time { return clock }
	kind := string(events.KindAppCPUThrottled)
	count := func() int {
		n := 0
		for _, e := range rec.events {
			if e.Kind == kind {
				n++
			}
		}
		return n
	}

	w.tick()
	if count() != 1 || len(mail.sent) != 1 {
		t.Fatalf("26%% over an hour: %d events, %d mails; want 1 and 1", count(), len(mail.sent))
	}
	for _, want := range []string{"its 1-core limit", "for 16 minutes of its 60 minutes of work (26%)", "Runtime settings"} {
		if !strings.Contains(rec.events[len(rec.events)-1].Detail, want) {
			t.Errorf("detail lacks %q: %s", want, rec.events[len(rec.events)-1].Detail)
		}
	}

	// Still throttled an hour later: said once, not once per check.
	clock = clock.Add(time.Hour)
	w.tick()
	if count() != 1 || len(mail.sent) != 1 {
		t.Errorf("an open event was raised again: %d events, %d mails", count(), len(mail.sent))
	}

	// Under a tenth for the hour: cleared.
	store.hour = share(0.05, 3600)
	clock = clock.Add(throttleCheckEvery)
	w.tick()
	if rec.cleared[kind] != 1 {
		t.Errorf("an hour under 10%% cleared %d times, want 1", rec.cleared[kind])
	}

	// Between the two thresholds nothing moves either way.
	store.hour = share(0.15, 3600)
	clock = clock.Add(throttleCheckEvery)
	w.tick()
	if count() != 1 {
		t.Errorf("15%% raised an event")
	}
}

// A one-minute burst is a cold start, not a verdict: an hour not yet measured
// is not judged.
func TestABurstIsNotAnHour(t *testing.T) {
	rec := &recordingStore{cleared: map[string]int{}}
	app := runningApp()
	served := time.Now().UTC().Add(-2 * time.Hour)
	app.ServingSince = &served
	store := &throttleStore{fakeStore: &fakeStore{apps: []database.App{app}, pending: map[string]bool{}},
		rec: rec, hour: database.CPUThrottling{Periods: 600, ThrottledPeriods: 600, MeasuredSeconds: 60}}
	w := New(store, runtime.NewRuntimes(&stubRuntime{workloads: online(0)}, nil),
		events.NewRecorder(rec, nil, nil), true, nil)
	w.tick()
	if len(rec.without(events.KindAppRestarted)) != 0 {
		t.Errorf("a one-minute burst recorded %v", rec.kinds())
	}
}

// A mostly idle app's hour holds few periods, and a short burst each minute is
// then a large share of them: under the floor nothing is raised, while an
// event already open still clears (RT-5).
func TestAFewPeriodsAreNotAVerdict(t *testing.T) {
	quiet := database.CPUThrottling{Periods: 360, ThrottledPeriods: 300, MeasuredSeconds: 3600}
	rec := &recordingStore{cleared: map[string]int{}}
	app := runningApp()
	served := time.Now().UTC().Add(-2 * time.Hour)
	app.ServingSince = &served
	store := &throttleStore{fakeStore: &fakeStore{apps: []database.App{app}, pending: map[string]bool{}},
		rec: rec, hour: quiet}
	w := New(store, runtime.NewRuntimes(&stubRuntime{workloads: online(0)}, nil),
		events.NewRecorder(rec, nil, nil), true, nil)
	w.tick()
	if len(rec.without(events.KindAppRestarted)) != 0 {
		t.Errorf("360 periods raised %v", rec.kinds())
	}

	open := &recordingStore{cleared: map[string]int{}}
	open.events = append(open.events, &database.AppEvent{Kind: string(events.KindAppCPUThrottled)})
	store = &throttleStore{fakeStore: &fakeStore{apps: []database.App{app}, pending: map[string]bool{}},
		rec: open, hour: database.CPUThrottling{Periods: 360, ThrottledPeriods: 0, MeasuredSeconds: 3600}}
	w = New(store, runtime.NewRuntimes(&stubRuntime{workloads: online(0)}, nil),
		events.NewRecorder(open, nil, nil), true, nil)
	w.tick()
	if open.cleared[string(events.KindAppCPUThrottled)] != 1 {
		t.Error("an open event did not clear on a quiet hour under the floor")
	}
}

func TestWorkWordsFallsBackToSeconds(t *testing.T) {
	if got := workWords(20); got != "20 seconds" {
		t.Errorf("workWords(20) = %q", got)
	}
	if got := workWords(600); got != "10 minutes" {
		t.Errorf("workWords(600) = %q", got)
	}
}

// A deploy starts the release's own hour: the old release's event clears, and
// nothing is judged until the new one has served an hour.
func TestADeployResetsTheThrottlingHour(t *testing.T) {
	rec := &recordingStore{cleared: map[string]int{}}
	rec.events = append(rec.events, &database.AppEvent{Kind: string(events.KindAppCPUThrottled)})
	app := runningApp()
	served := time.Now().UTC().Add(-10 * time.Minute)
	app.ServingSince = &served
	store := &throttleStore{fakeStore: &fakeStore{apps: []database.App{app}, pending: map[string]bool{}},
		rec: rec, hour: share(0.9, 3600)}
	w := New(store, runtime.NewRuntimes(&stubRuntime{workloads: online(0)}, nil),
		events.NewRecorder(rec, nil, nil), true, nil)
	w.tick()
	if rec.cleared[string(events.KindAppCPUThrottled)] != 1 {
		t.Errorf("the previous release's event was not cleared")
	}
	if n := len(rec.events); n != 1 {
		t.Errorf("a release serving for ten minutes was judged: %d events", n)
	}
}
