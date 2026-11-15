package client

import (
	"context"
	"errors"
	"time"
)

// Waiting on the platform, in the one place it happens.
//
// Three commands wait: `toft restart --wait` on an app's status, `toft job run` on
// a run's nine states, `toft task run` on a task run's four. They wait on three
// different shapes and agree on nothing except the loop — and a loop written
// three times is how three commands drift into three opinions about what a
// timeout means and how long a blip is tolerated for.

// ErrPollTimeout is the deadline passing with the thing still unsettled.
//
// Deliberately not an error about the platform: nothing failed, and only the
// caller knows what the silence means or what to print about it. `toft restart
// --wait` reports the last status it read and names `toft events`; `toft job run`
// names the run id, which is still going and can still be looked at.
var ErrPollTimeout = errors.New("timed out waiting for the platform")

// DefaultPollInterval is how long between reads when a caller does not say.
//
// Two seconds is the SPA's own cadence for the same rows. Faster buys nothing —
// none of these states changes on a sub-second boundary — and costs an install
// a request per second per waiting pipeline.
const DefaultPollInterval = 2 * time.Second

// toleratedUnreachable is how many *consecutive* failures to reach the install
// are ridden out before the wait gives up on it.
//
// A read that did not happen is not an answer. Every other error here is the
// platform saying something — a 403 is a refusal, a 404 is a missing run — and
// is returned at once. An unreachable install is the one case where the client
// learned nothing at all, and a job that runs for twenty minutes should not be
// abandoned because one poll met a closed connection. Persist and it is still
// exit 4, roughly six seconds later.
const toleratedUnreachable = 3

// PollOptions bounds a wait.
type PollOptions struct {
	// Interval between reads. Zero means DefaultPollInterval.
	Interval time.Duration
	// Timeout is the whole wait. Zero means the context is the only bound,
	// which is what a job run wants: a job that takes an hour is a job, not a
	// hang, and the person who started it can still Ctrl-C.
	Timeout time.Duration
}

// Poll calls check on an interval until it reports done.
//
// check returns (true, nil) when whatever is being waited on has settled. Its
// error ends the wait, except for an unreachable install — see
// toleratedUnreachable. The context ends it too, which is Ctrl-C, and that is
// reported as the context's own error so `report` can tell it from a timeout.
func Poll(ctx context.Context, opts PollOptions, check func(context.Context) (bool, error)) error {
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	// The first read happens immediately. A restart that completed inside the
	// wake hold, or a job that was already finished when the wait began, must
	// not be reported one interval later than it was true.
	misses := 0
	for {
		done, err := check(ctx)
		switch {
		case err == nil:
			misses = 0
			if done {
				return nil
			}
		case isUnreachable(err):
			misses++
			if misses > toleratedUnreachable {
				return err
			}
		default:
			return err
		}

		select {
		case <-ctx.Done():
			return pollContextErr(ctx, opts)
		case <-time.After(interval):
		}
	}
}

// pollContextErr separates "the wait ran out" from "somebody pressed Ctrl-C".
// They are different sentences and different exit codes.
func pollContextErr(ctx context.Context, opts PollOptions) error {
	if opts.Timeout > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrPollTimeout
	}
	return ctx.Err()
}

func isUnreachable(err error) bool {
	var u *UnreachableError
	return errors.As(err, &u)
}
