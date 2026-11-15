package builder

import (
	"encoding/json"
	"time"

	"toft/internal/database"
	"toft/internal/deploysteps"
)

// ProgressEvent is one deploy, as something watching it sees it at a moment.
//
// It is a **snapshot, not a delta**: every frame states the deploy's status,
// phase and step in full. A watcher that connects late, or misses a frame
// because its channel was full, is still correct after the next one — which
// matters because the two clients for this are a browser tab that may have
// been backgrounded and a CLI on somebody's laptop wifi.
//
// This shape is a contract with clients this repository does not contain. Its
// field names and its step and phase vocabulary (`internal/deploysteps`) are
// the CLI's API; add fields freely, and treat renaming one as the breaking
// change it is.
type ProgressEvent struct {
	DeploymentID string `json:"deployment_id"`
	// Status is the deployment's own status — pending, building, success,
	// failed. A watcher stops when it reads success or failed and needs no
	// other signal to know the deploy is over.
	Status string `json:"status"`
	// Phase and Step are where it is. Phase is derived from Step and sent
	// anyway, so that no client has to carry the mapping.
	Phase string    `json:"phase"`
	Step  string    `json:"step"`
	At    time.Time `json:"at"`
	// Message is why, on a terminal frame that failed. Empty otherwise: a
	// running deploy's narration is the build log, which is a different stream
	// for a reason — one is a progress display and the other is output.
	//
	// **In full, not clipped to a line.** It used to carry firstLine(msg),
	// which on the one failure that says the most — a dependency refusal naming
	// packages, advisories and the versions that fix them — threw away
	// everything but the headline and left it recoverable only from the build
	// log. The client already sizes its scanner for a message it does not
	// bound.
	Message string `json:"message,omitempty"`
	// Reason is Message's branchable half: why this failed, as a key from
	// `deploysteps`. Empty on a frame that did not fail, and on a deploy that
	// predates the vocabulary — which means *unclassified*, and which no
	// client may read as any particular cause.
	//
	// A key beside the sentence rather than instead of it. The sentence is for
	// a person; the key is for the caller that has to decide what to do next
	// without matching on prose that ~30 call sites are free to reword.
	Reason string `json:"reason,omitempty"`
	// Code is which refusal inside Reason, when the failure has a finer one
	// (storage_unknown under storage_refused) — the row's failure_code.
	Code string `json:"code,omitempty"`
	// Warnings is what the deploy noticed and carried on past — a table in
	// toft.toml that no reader names, and anything else ignored rather
	// than refused. Present on a *successful* frame as readily as a failed one,
	// which is the whole point: the deploy worked, and something in it did not
	// mean what its author thought.
	//
	// Absent when there are none, and absent from an install older than the
	// field — which a client must read as "this install does not report
	// warnings", never as "there were none".
	Warnings []string `json:"warnings,omitempty"`
	// WarningDetails is each of Warnings that has a code, as toft check's
	// warning_details is: {code, table, message}. Absent when none has one.
	WarningDetails []Warning `json:"warning_details,omitempty"`
}

// Terminal reports whether this frame is the last one for its deployment.
func (e ProgressEvent) Terminal() bool {
	return e.Status == database.DeployStatusSuccess || e.Status == database.DeployStatusFailed
}

// NewProgressEvent assembles a frame, deriving the phase so that no caller can
// assemble one that disagrees with the step it carries.
func NewProgressEvent(deployID, status string, step BuildStep, message string) ProgressEvent {
	return ProgressEvent{
		DeploymentID: deployID,
		Status:       status,
		Phase:        string(deploysteps.PhaseOf(string(step))),
		Step:         string(step),
		At:           time.Now().UTC(),
		Message:      message,
	}
}

// NewFailureEvent is the terminal frame of a deploy that failed, carrying both
// halves of why.
//
// Separate from NewProgressEvent rather than a fifth argument on it, because
// every other caller of that function is publishing a frame that cannot have a
// reason — a deploy in flight has not failed — and an argument they would all
// pass empty is one a new failure site could pass empty too.
func NewFailureEvent(deployID string, step BuildStep, reason deploysteps.Reason, message string) ProgressEvent {
	event := NewProgressEvent(deployID, database.DeployStatusFailed, step, message)
	event.Reason = string(reason)
	return event
}

// SnapshotProgress describes a deployment as it stands right now, for a
// watcher that has only just arrived. It reads the row rather than the live
// stream, so it is also the right answer for a deploy that finished before
// anyone thought to watch.
//
// **It carries why, and it has to.** A snapshot is what a watcher receives when
// it reconnects, when it attaches with `--watch` to a deploy started earlier,
// and when the build was already over before anyone subscribed — which is to
// say, in every case where the live terminal frame is the one thing it missed.
// Reading the failure off the row rather than off a frame nobody kept is what
// makes "every frame states the deploy in full" true of this one too.
func SnapshotProgress(deploy *database.Deployment) ProgressEvent {
	return ProgressEvent{
		DeploymentID:   deploy.ID,
		Status:         deploy.Status,
		Phase:          string(deploysteps.PhaseOf(deploy.CurrentStep)),
		Step:           deploy.CurrentStep,
		At:             time.Now().UTC(),
		Message:        deploy.FailureMessage,
		Reason:         deploy.FailureReason,
		Code:           deploy.FailureCode,
		Warnings:       DecodeWarnings(deploy.Warnings),
		WarningDetails: DecodeWarningDetails(deploy.Warnings),
	}
}

// EncodeWarnings and DecodeWarnings are the one crossing between the column and
// the wire, so nothing else has to know the column holds JSON.
//
// The column is a JSON array whose entries are a sentence, or — for a warning
// with a code — {code, table, message}, so a deploy's result can carry
// warning_details as toft check's does (BB-037, fourth 2026-11-03 black-box
// list). Rows written before the objects hold sentences alone and read the
// same.
//
// Decoding is deliberately forgiving: a row this build cannot read is a row
// written by a version it does not know, and losing the warnings is better than
// failing the snapshot that carries the deploy's status. Nothing branches on
// them.
func EncodeWarnings(warnings []string) string {
	details := make([]Warning, 0, len(warnings))
	for _, w := range warnings {
		details = append(details, Warning{Message: w})
	}
	return EncodeWarningDetails(details)
}

// EncodeWarningDetails stores each warning as a sentence when it has no code,
// and as an object when it has one.
func EncodeWarningDetails(warnings []Warning) string {
	if len(warnings) == 0 {
		return ""
	}
	entries := make([]any, 0, len(warnings))
	for _, w := range warnings {
		if w.Code == "" {
			entries = append(entries, w.Message)
		} else {
			entries = append(entries, w)
		}
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return ""
	}
	return string(raw)
}

// decodeStoredWarnings is every entry, a sentence read as a Warning with no
// code.
func decodeStoredWarnings(stored string) []Warning {
	if stored == "" {
		return nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(stored), &raw); err != nil {
		return nil
	}
	out := make([]Warning, 0, len(raw))
	for _, r := range raw {
		var sentence string
		if err := json.Unmarshal(r, &sentence); err == nil {
			out = append(out, Warning{Message: sentence})
			continue
		}
		var w Warning
		if err := json.Unmarshal(r, &w); err == nil && w.Message != "" {
			out = append(out, w)
		}
	}
	return out
}

// DecodeWarnings is the sentences, every one.
func DecodeWarnings(stored string) []string {
	ws := decodeStoredWarnings(stored)
	if len(ws) == 0 {
		return nil
	}
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Message)
	}
	return out
}

// DecodeWarningDetails is the warnings that have a code, in order — what a
// progress frame carries as warning_details.
func DecodeWarningDetails(stored string) []Warning {
	var out []Warning
	for _, w := range decodeStoredWarnings(stored) {
		if w.Code != "" {
			out = append(out, w)
		}
	}
	return out
}

// SubscribeProgress returns a channel of this deployment's step changes.
//
// Deliberately separate from SubscribeLogs rather than multiplexed onto it.
// The log stream is bytes an app's build produced, it is consumed by a viewer
// that renders them as text, and its framing is already a contract with the
// SPA. This one is structured state consumed by anything drawing a progress
// display. Putting typed frames on the log channel would make every consumer of
// either parse both.
func (b *Builder) SubscribeProgress(deploymentID string) chan ProgressEvent {
	b.mu.Lock()
	defer b.mu.Unlock()

	ch := make(chan ProgressEvent, 32)
	b.watchers[deploymentID] = append(b.watchers[deploymentID], ch)
	return ch
}

func (b *Builder) UnsubscribeProgress(deploymentID string, ch chan ProgressEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()

	watchers := b.watchers[deploymentID]
	for i, w := range watchers {
		if w == ch {
			b.watchers[deploymentID] = append(watchers[:i], watchers[i+1:]...)
			break
		}
	}
	if len(b.watchers[deploymentID]) == 0 {
		delete(b.watchers, deploymentID)
	}
	close(ch)
}

// PublishProgress tells everyone watching where a deploy has got to.
//
// Exported because the builder is not the only thing that moves a deploy
// along; see StepLog. Non-blocking on a full channel for the reason broadcast
// is — a watcher that cannot keep up must never be able to stall a deploy —
// and safe to drop for the reason ProgressEvent is a snapshot: the next frame
// corrects it.
func (b *Builder) PublishProgress(deployID, status string, step BuildStep, message string) {
	b.PublishEvent(NewProgressEvent(deployID, status, step, message))
}

// PublishEvent is the same broadcast for a frame the caller has already
// assembled — the terminal failure frame, which carries a reason no derived
// four-argument form could supply.
func (b *Builder) PublishEvent(event ProgressEvent) {
	if b == nil {
		return
	}
	deployID := event.DeploymentID

	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, ch := range b.watchers[deployID] {
		select {
		case ch <- event:
		default:
		}
	}
}
