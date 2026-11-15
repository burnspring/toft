package client

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestPollReturnsAsSoonAsItIsDone(t *testing.T) {
	calls := 0
	err := Poll(context.Background(), PollOptions{Interval: time.Millisecond}, func(context.Context) (bool, error) {
		calls++
		return true, nil
	})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	// The first read is immediate: a restart that completed inside the wake
	// hold must not be reported an interval after it was true.
	if calls != 1 {
		t.Fatalf("checked %d times, want 1", calls)
	}
}

func TestPollKeepsReadingUntilSettled(t *testing.T) {
	calls := 0
	err := Poll(context.Background(), PollOptions{Interval: time.Millisecond}, func(context.Context) (bool, error) {
		calls++
		return calls == 3, nil
	})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if calls != 3 {
		t.Fatalf("checked %d times, want 3", calls)
	}
}

func TestPollTimesOutRatherThanHanging(t *testing.T) {
	err := Poll(context.Background(), PollOptions{Interval: time.Millisecond, Timeout: 20 * time.Millisecond},
		func(context.Context) (bool, error) { return false, nil })
	if !errors.Is(err, ErrPollTimeout) {
		t.Fatalf("got %v, want ErrPollTimeout", err)
	}
}

func TestPollReturnsThePlatformsRefusal(t *testing.T) {
	refusal := &Error{Status: 409, Code: "already_running", Message: "a run is already going"}
	err := Poll(context.Background(), PollOptions{Interval: time.Millisecond}, func(context.Context) (bool, error) {
		return false, refusal
	})
	// Every error but an unreachable install is the platform saying something,
	// and it is returned whole rather than retried into a timeout.
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != "already_running" {
		t.Fatalf("got %v, want the 409 back unchanged", err)
	}
}

func TestPollRidesOutABlipAndCarriesOn(t *testing.T) {
	calls := 0
	err := Poll(context.Background(), PollOptions{Interval: time.Millisecond}, func(context.Context) (bool, error) {
		calls++
		if calls <= toleratedUnreachable {
			return false, &UnreachableError{URL: "https://install", Err: fmt.Errorf("connection refused")}
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("a wait should survive %d unreachable reads, got %v", toleratedUnreachable, err)
	}
}

func TestPollGivesUpOnAnInstallThatStaysAway(t *testing.T) {
	err := Poll(context.Background(), PollOptions{Interval: time.Millisecond}, func(context.Context) (bool, error) {
		return false, &UnreachableError{URL: "https://install", Err: fmt.Errorf("connection refused")}
	})
	// Persistently unreachable is still exit 4, not a timeout: the client knows
	// which of those two happened and must not blur them.
	var unreachable *UnreachableError
	if !errors.As(err, &unreachable) {
		t.Fatalf("got %v, want UnreachableError", err)
	}
}

func TestPollAReachableInstallResetsTheTolerance(t *testing.T) {
	calls := 0
	err := Poll(context.Background(), PollOptions{Interval: time.Millisecond}, func(context.Context) (bool, error) {
		calls++
		// Alternate: never toleratedUnreachable+1 in a row, so this must not
		// give up however long it goes on.
		if calls%2 == 1 && calls < 12 {
			return false, &UnreachableError{URL: "https://install", Err: fmt.Errorf("blip")}
		}
		return calls >= 12, nil
	})
	if err != nil {
		t.Fatalf("intermittent blips should not end a wait, got %v", err)
	}
}

func TestPollStopsOnCtrlC(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	err := Poll(ctx, PollOptions{Interval: time.Millisecond, Timeout: time.Minute},
		func(context.Context) (bool, error) { return false, nil })
	// Ctrl-C is context.Canceled and not a timeout — `report` maps them to
	// different exits and prints different things.
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrPollTimeout) {
		t.Fatal("a cancelled wait must not be reported as a timeout")
	}
}
