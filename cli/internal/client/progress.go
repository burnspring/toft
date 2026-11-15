package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Watching one deploy through the progress stream.
//
// Three rules hold here, and the API was shaped so they could:
//
//   - **The terminal frame is the end signal**, not the connection closing. A
//     stream that goes quiet is a deploy still working, which is why the server
//     heartbeats and why nothing here times a build out.
//   - **A dropped connection is not a failed deploy.** Frames are snapshots —
//     every one states status, phase and step in full — so reconnecting loses
//     nothing, and a deploy that finished while disconnected answers
//     immediately from the first frame of the new connection.
//   - **An unknown phase or step renders as its key.** The step vocabulary is
//     designed so adding one is safe, and a laptop's `toft` is older than the
//     install it talks to more often than not. The client must not be the thing
//     that makes a safe change unsafe.

// ProgressEvent is one frame, exactly as the server sends it.
//
// Restated here rather than imported from internal/builder, which pulls in the
// runtime, Kubernetes and the database. The *vocabulary* — which steps exist,
// what order they come in, which phase each belongs to — comes from
// toft/internal/deploysteps, which is a leaf package precisely so that both
// ends can share it.
type ProgressEvent struct {
	DeploymentID string    `json:"deployment_id"`
	Status       string    `json:"status"`
	Phase        string    `json:"phase"`
	Step         string    `json:"step"`
	At           time.Time `json:"at"`
	Message      string    `json:"message,omitempty"`
	// Reason is Message's branchable half on a frame that failed — a key from
	// toft/internal/deploysteps, restated on the wire for the reason the
	// steps are.
	//
	// **Empty from an install older than the field, and that is not an error.**
	// A laptop's `toft` being newer than the install it talks to is as ordinary
	// as the reverse; absent means unclassified, and nothing here may read it
	// as any particular cause. The phase and step are still the answer they
	// always were.
	Reason string `json:"reason,omitempty"`
	// Code is which refusal inside Reason, when the failure has a finer one
	// (storage_unknown under storage_refused) — the code `toft check` names
	// for the same tree. Absent from an install older than the field.
	Code string `json:"code,omitempty"`
	// Warnings is what the deploy noticed and carried on past — a table in
	// toft.toml no reader names, and anything else ignored rather than
	// refused. Carried on the terminal frame of a *successful* deploy as
	// readily as a failed one, which is the case that needs it: nothing failed,
	// so nothing sends anybody to the build log.
	//
	// **Empty from an install older than the field means "this install does not
	// report warnings", never "there were none".** Same rule as Reason above,
	// and the same reason: a laptop's `toft` being newer than the install it
	// talks to is as ordinary as the reverse.
	Warnings []string `json:"warnings,omitempty"`
	// WarningDetails is each of Warnings that has a code — {code, table,
	// message}, as toft check's warning_details is. Passed through verbatim,
	// so a field added beside these still reaches a --json reader. Absent from
	// an install older than the field, and when no warning has a code.
	WarningDetails []json.RawMessage `json:"warning_details,omitempty"`
}

// Deploy statuses, as they come off the wire.
const (
	StatusPending  = "pending"
	StatusBuilding = "building"
	StatusSuccess  = "success"
	StatusFailed   = "failed"
)

// Terminal reports whether this is the last frame for its deployment.
func (e ProgressEvent) Terminal() bool {
	return e.Status == StatusSuccess || e.Status == StatusFailed
}

// watchGiveUp is how long a watch goes on retrying without one successful
// connection before it ends unreachable. Five minutes covers an install
// restarting; a variable so a test can shorten it.
var watchGiveUp = 5 * time.Minute

// WatchProgress streams one deploy's frames to onFrame until a terminal frame
// arrives, and reconnects with backoff in between.
//
// It returns the terminal frame. A context that is cancelled — Ctrl-C — returns
// the last frame seen and ctx.Err(); the deploy itself carries on, which is the
// honest behaviour and what the caller says on the way out. There is no cancel
// route for a deploy, and inventing a client-side illusion of one would be
// worse than the sentence.
//
// **Reconnecting is bounded, by time without a connection** — never by the
// stream itself: a connected stream that is quiet is a build step working,
// kept alive by the server's heartbeat, and may run twenty minutes. What ends
// the watch is an answer that will not change (a refusal the install says not
// to retry, a host name that does not resolve) at once, and watchGiveUp with
// no stream opened, as an *UnreachableError. It used to reconnect for ever and
// in silence (security review 2026-11-14, CS-4); OnReconnect is how the caller
// says so now.
func (c *Client) WatchProgress(ctx context.Context, appID, deployID string, onFrame func(ProgressEvent)) (ProgressEvent, error) {
	path := fmt.Sprintf("/api/v1/apps/%s/deployments/%s/progress", appID, deployID)

	var last, explained ProgressEvent
	backoff := time.Second
	lastGood := time.Now()
	var said time.Time
	for {
		terminal, connected, err := c.streamProgress(ctx, path, func(e ProgressEvent) {
			last = e
			if e.Reason != "" || (e.Status == StatusFailed && e.Message != "") {
				explained = e
			}
			onFrame(e)
		})
		if terminal {
			return withFailureDetail(last, explained), nil
		}
		if ctx.Err() != nil {
			return last, ctx.Err()
		}
		if watchEnds(err) {
			return last, err
		}
		if connected {
			lastGood = time.Now()
			backoff = time.Second
			// A stream that worked has dropped: said once per outage, not once
			// per retry, and not more than twice a minute however often a
			// stream opens and drops.
			if c.OnReconnect != nil && time.Since(said) > 30*time.Second {
				said = time.Now()
				c.OnReconnect(err)
			}
		} else if time.Since(lastGood) >= watchGiveUp {
			var unreachable *UnreachableError
			if !errors.As(err, &unreachable) {
				err = &UnreachableError{URL: c.BaseURL, Err: err}
			}
			return last, err
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

// watchEnds is whether an error ends the watch at once: an answer that will
// be the same on every retry.
func watchEnds(err error) bool {
	if err == nil {
		return false
	}
	// A refusal is not a blip: a revoked key or an app that has gone away
	// will refuse identically forever, and retrying it is a client that
	// hangs instead of one that says why.
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.Unauthorized() || apiErr.NotFound() || !apiErr.ShouldRetry()
	}
	var unreachable *UnreachableError
	if errors.As(err, &unreachable) {
		return !unreachable.ShouldRetry()
	}
	var tooLong *FrameTooLongError
	return errors.As(err, &tooLong)
}

// FrameTooLongError is a progress frame longer than this client reads. The same
// frame would come back on every reconnect, so the watch ends and names where
// the outcome can be read instead.
type FrameTooLongError struct{ DeploymentID string }

func (e *FrameTooLongError) Error() string {
	return "a progress frame was too long to read; the deploy's outcome is in `toft deployments`"
}

func (e *FrameTooLongError) Unwrap() error { return bufio.ErrTooLong }

// streamProgress runs one connection. It reports whether it saw the terminal
// frame, and whether the stream opened at all (a 200); anything else — a clean
// EOF included — is a dropped connection and the caller reconnects.
func (c *Client) streamProgress(ctx context.Context, path string, onFrame func(ProgressEvent)) (terminal, connected bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return false, false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	c.authorize(req)

	// No timeout: a build step can take twenty minutes with nothing to say, and
	// the server's heartbeat is what proves the connection is alive.
	resp, err := (&http.Client{Transport: c.HTTP.Transport, CheckRedirect: c.HTTP.CheckRedirect}).Do(req)
	if err != nil {
		return false, false, &UnreachableError{URL: c.BaseURL, Err: err}
	}
	c.Observe(resp)
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return false, false, errorFrom(resp)
	}

	scanner := bufio.NewScanner(resp.Body)
	// A frame is small, but a failure message carries the reason a deploy died
	// and is not bounded by anything the client controls.
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		// Comments are the heartbeat, and blank lines separate frames. Neither
		// carries data; both prove the connection is alive.
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event ProgressEvent
		if err := json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &event); err != nil {
			// A field of a different type than this client's — a newer
			// install's — is no reason to drop the frame: Go fills every other
			// field, status included, and a dropped terminal frame came back
			// on every reconnect for ever.
			var typeErr *json.UnmarshalTypeError
			if !errors.As(err, &typeErr) {
				continue
			}
		}
		onFrame(event)
		if event.Terminal() {
			return true, true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return false, true, &FrameTooLongError{}
		}
		return false, true, err
	}
	return false, true, nil
}

// StreamLines reads a plain-text stream line by line — the build log and the
// runtime log both. Returns when the server closes it or the context ends.
func (c *Client) StreamLines(ctx context.Context, path string, onLine func(string)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	c.authorize(req)
	resp, err := (&http.Client{Transport: c.HTTP.Transport, CheckRedirect: c.HTTP.CheckRedirect}).Do(req)
	if err != nil {
		return &UnreachableError{URL: c.BaseURL, Err: err}
	}
	c.Observe(resp)
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return errorFrom(resp)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		onLine(scanner.Text())
	}
	return scanner.Err()
}

// withFailureDetail is the terminal frame of a failed deploy with the why
// filled in from the frame that said it. A step's failure frame can carry the
// reason and message while the terminal frame after it carries neither, and
// the terminal frame is what a --json result is built from — so the result
// held a log tail and no sentence (BB-074, sixth 2026-11-03 list). The
// terminal frame's own fields win where it has them.
func withFailureDetail(final, explained ProgressEvent) ProgressEvent {
	if final.Status != StatusFailed {
		return final
	}
	if final.Reason == "" {
		final.Reason = explained.Reason
	}
	if final.Code == "" {
		final.Code = explained.Code
	}
	if final.Message == "" {
		final.Message = explained.Message
	}
	if final.Phase == "" {
		final.Phase = explained.Phase
	}
	if final.Step == "" {
		final.Step = explained.Step
	}
	return final
}
