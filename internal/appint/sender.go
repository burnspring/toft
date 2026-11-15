package appint

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"toft/internal/database"
	"toft/internal/telemetry"
)

// Queue shape. Small on purpose: this is a bounded in-process queue, not a job
// system. If an install needs more than this can absorb, what it needs is a
// queue with its own durability story, not a bigger channel.
const (
	QueueDepth = 256
	Workers    = 8

	// DeliverTimeout bounds one attempt at a transport.
	DeliverTimeout = 60 * time.Second
	// RetryDelay is the pause before the single retry.
	RetryDelay = 5 * time.Second

	// PerAppDepth and PerAppBytes are one app's share of the queue. The queue
	// is the install's, and every job run is a credential of its own, so the
	// per-token rate limit does not bound what one app holds: without a share,
	// one app sending through a slow far side fills all 256 slots and every
	// other app's send — a page included — is refused. Keyed on the app, never
	// the token, for that reason.
	PerAppDepth = QueueDepth / Workers
	// PerAppBytes bounds the payload memory one app holds in the queue. A
	// single send of any size is always accepted when the app holds nothing,
	// so the largest file an app may send still goes.
	PerAppBytes = 64 << 20
)

// ErrQueueFull is what a caller gets when the queue is at depth. It becomes a
// 429 with Retry-After, never a silent drop: a 202 for a message that was
// discarded is worse than a refusal, because the app stops watching.
var ErrQueueFull = errors.New("the send queue is full")

// File is one attachment, in memory, on its way to a transport. It never
// reaches the ledger — only its name, size, type and digest do.
type File struct {
	Name    string
	Type    string
	Content []byte
}

// Payload is the message itself. It lives in memory for the life of the job and
// is deliberately not read back out of the ledger: the body is stored only as a
// hash unless an install opted into retaining it, and a delivery path that
// depended on retention would work differently on two installs.
type Payload struct {
	// Slack.
	Text    string
	Title   string
	Comment string
	// Email.
	Subject string
	To      []string
	CC      []string
	BCC     []string
	HTML    string
	// On-call. Severity is what a page says about itself; DedupKey is what
	// closes it, and it is namespaced with the app id before it leaves so an
	// app can only resolve what it triggered.
	Severity string
	DedupKey string
	// Both.
	Files []File
}

// size is what the payload holds in memory while it waits.
func (p Payload) size() int64 {
	n := int64(len(p.Text) + len(p.Title) + len(p.Comment) + len(p.Subject) + len(p.HTML))
	for _, f := range p.Files {
		n += int64(len(f.Content))
	}
	return n
}

// Job is one accepted send on its way to a transport.
type Job struct {
	Send       *database.IntegrationSend
	Payload    Payload
	Capability string
	AppStatus  string
	// AppName and BaseURL are what the transports need to attribute the message
	// to the app — the frame that says an application sent this, not the
	// platform. Carried on the job because a worker must not have to go back to
	// the database for them mid-delivery.
	AppName string
	AppSlug string
	BaseURL string

	// ConnectionID is the connection this send goes through, resolved once at
	// acceptance and carried.
	//
	// Carried rather than re-resolved for the reason the ledger records the
	// destination before the send is attempted: a worker that looked it up
	// again could deliver somewhere other than what the record says, and an
	// incident asking "where did that go" would get two answers. The gates are
	// re-checked at delivery — off means now — but where a send was going was
	// decided when it was accepted.
	ConnectionID string
	// Destination is where inside the connection, already resolved through its
	// default. Empty for a welded kind.
	Destination database.Destination
}

// DeliverFunc hands a job to the right transport. Supplied by the wiring layer,
// which is the only place that knows about both slack and mail — this package
// deliberately knows about neither.
type DeliverFunc func(ctx context.Context, j Job) error

// Sender accepts sends, records them, and delivers them out of band.
type Sender struct {
	store   Store
	deliver DeliverFunc
	jobs    chan Job
	wg      sync.WaitGroup

	// onFailure is told about a send that did not go out, so the wiring layer
	// can write the app_events row an owner sees on the tab they already read.
	// Only failures: forty successful sends a day in that feed would drown the
	// one deploy failure it exists to surface.
	onFailure func(j Job, cause string)

	stopOnce sync.Once
	stopped  chan struct{}

	// held is each app's share of the queue: jobs accepted and not yet
	// finished, and the payload bytes they carry.
	mu   sync.Mutex
	held map[string]share
}

type share struct {
	n     int
	bytes int64
}

// NewSender builds the queue. It does not start it.
func NewSender(store Store, deliver DeliverFunc) *Sender {
	return &Sender{
		store:   store,
		deliver: deliver,
		jobs:    make(chan Job, QueueDepth),
		stopped: make(chan struct{}),
		held:    map[string]share{},
	}
}

// OnFailure registers the failure hook. Optional and nil-safe.
func (s *Sender) OnFailure(fn func(j Job, cause string)) {
	if s != nil {
		s.onFailure = fn
	}
}

// Start launches the workers.
func (s *Sender) Start() {
	if s == nil {
		return
	}
	for i := 0; i < Workers; i++ {
		s.wg.Add(1)
		go s.work()
	}
}

// Stop stops accepting and waits for what is in flight. Drain, not close: a
// message already accepted has been promised a record, and abandoning it at
// shutdown would leave a `queued` row that only the next startup sweep could
// explain.
func (s *Sender) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stopped) })
	s.wg.Wait()
}

// Enqueue accepts a job that has already been recorded as queued.
//
// Non-blocking by design. The alternative — waiting for a slot — would make an
// app's send call sit on the platform's queue depth, which is the one thing the
// whole design promises it will not do.
//
// An app past its share (PerAppDepth jobs, or PerAppBytes of payload once it
// holds one) is refused with an error that wraps ErrQueueFull and says the
// backlog is its own.
func (s *Sender) Enqueue(j Job) error {
	if s == nil {
		return errors.New("app integrations are not available on this install")
	}
	app := j.Send.AppID
	size := j.Payload.size()
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.held[app]
	if h.n >= PerAppDepth {
		return fmt.Errorf("%w: this app already has %d sends waiting", ErrQueueFull, h.n)
	}
	if h.n > 0 && h.bytes+size > PerAppBytes {
		return fmt.Errorf("%w: this app already has %d MiB of sends waiting", ErrQueueFull, h.bytes>>20)
	}
	select {
	case s.jobs <- j:
		s.held[app] = share{n: h.n + 1, bytes: h.bytes + size}
		return nil
	default:
		return ErrQueueFull
	}
}

// release returns a finished job's share.
func (s *Sender) release(j Job) {
	app := j.Send.AppID
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.held[app]
	h.n--
	h.bytes -= j.Payload.size()
	if h.n <= 0 {
		delete(s.held, app)
		return
	}
	s.held[app] = h
}

func (s *Sender) work() {
	defer s.wg.Done()
	for {
		select {
		case <-s.stopped:
			// Drain what is already accepted, then go.
			for {
				select {
				case j := <-s.jobs:
					s.handle(j)
				default:
					return
				}
			}
		case j := <-s.jobs:
			s.handle(j)
		}
	}
}

// handle re-checks the gates and delivers.
func (s *Sender) handle(j Job) {
	defer s.release(j)
	// Off means *now*. An administrator who detached an integration a second
	// ago has stopped this send, not merely the next one — and a send stopped
	// this way is `blocked` rather than `failed`, because "somebody turned it
	// off" and "Slack returned 500" are different facts and an owner reading
	// the ledger deserves to see which one happened.
	//
	// The lock is re-read too, and only the lock: the app's routes refuse a
	// locked app at the token, but up to a queue's depth of sends accepted
	// before the lock would otherwise go out after it. Its *status* is the
	// snapshot taken at acceptance and stays so — a message an app sends as
	// its last act before a stop was accepted while it ran (security review
	// 2026-11-15, LSA-3).
	app, err := s.store.GetApp(j.Send.AppID)
	if err != nil {
		s.complete(j, database.SendFailed, "could not re-check this app: "+err.Error())
		return
	}
	if app.Locked() {
		s.complete(j, database.SendBlocked, "this app was locked before the send went out")
		return
	}
	decision, err := Check(s.store, j.Send.AppID, j.AppStatus, j.Capability)
	if err != nil {
		s.complete(j, database.SendFailed, "could not re-check this app's integration: "+err.Error())
		return
	}
	if !decision.Allowed {
		s.complete(j, database.SendBlocked, decision.Reason)
		return
	}

	if err := s.attempt(j); err != nil {
		s.complete(j, database.SendFailed, err.Error())
		return
	}
	s.complete(j, database.SendSent, "")
}

// attempt delivers, with exactly one retry for a failure that might not repeat.
//
// One and not more: a report that arrives twice is a worse outcome than one
// that arrives once and is recorded as failed, and the record — not the
// message — is what a workflow relies on. A 4xx from a transport never retries,
// because the thing that was wrong with the message is still wrong.
func (s *Sender) attempt(j Job) error {
	ctx, cancel := context.WithTimeout(context.Background(), DeliverTimeout)
	err := s.deliver(ctx, j)
	cancel()
	if err == nil || !IsRetryable(err) {
		return err
	}

	select {
	case <-time.After(RetryDelay):
	case <-s.stopped:
		return err
	}

	ctx, cancel = context.WithTimeout(context.Background(), DeliverTimeout)
	defer cancel()
	return s.deliver(ctx, j)
}

func (s *Sender) complete(j Job, status, cause string) {
	// Counted here rather than at acceptance, because the question a dashboard
	// asks is "how many got out" and a row that was accepted and then blocked
	// is not one. The status is the ledger's own word, so the metric and the
	// record cannot disagree about what happened.
	telemetry.RecordIntegrationSend(context.Background(), j.Send.Kind, j.Send.Action, status)

	if err := s.store.CompleteSend(j.Send.ID, status, cause); err != nil {
		slog.Error("recording the outcome of an app send failed",
			"send_id", j.Send.ID, "app_id", j.Send.AppID, "status", status, "error", err)
	}
	switch status {
	case database.SendSent:
		slog.Info("app send delivered",
			"send_id", j.Send.ID, "app_id", j.Send.AppID, "kind", j.Send.Kind)
	default:
		slog.Error("app send did not go out",
			"send_id", j.Send.ID, "app_id", j.Send.AppID,
			"kind", j.Send.Kind, "status", status, "cause", cause)
		if s.onFailure != nil {
			s.onFailure(j, cause)
		}
	}
}

// --------------------------------------------------------------- retryable

type retryableError struct{ err error }

func (r retryableError) Error() string   { return r.err.Error() }
func (r retryableError) Unwrap() error   { return r.err }
func (r retryableError) Retryable() bool { return true }

// Retryable marks a transport failure as one worth a second attempt — a 5xx, a
// timeout, a rate limit. A transport that does not mark anything simply never
// retries, which is the safe direction.
func Retryable(err error) error {
	if err == nil {
		return nil
	}
	return retryableError{err: err}
}

// IsRetryable reports whether Retryable wrapped this error anywhere in its
// chain.
func IsRetryable(err error) bool {
	var r interface{ Retryable() bool }
	return errors.As(err, &r) && r.Retryable()
}

// ------------------------------------------------------------------ digest

// Digest is the body hash the ledger stores instead of the body. It proves two
// sends were identical and matches a leaked message back to a send, without
// turning the ledger into a copy of every payload the install has transmitted.
//
// Each part is written behind its length, so the boundary between parts is
// part of what is hashed: subject "Report" with body "A: done" and subject
// "ReportA" with body ": done" are two messages and two digests. Size is the
// parts' bytes alone.
func Digest(parts ...string) (sum string, size int) {
	h := sha256.New()
	var n [8]byte
	for _, p := range parts {
		binary.BigEndian.PutUint64(n[:], uint64(len(p)))
		h.Write(n[:])
		h.Write([]byte(p))
		size += len(p)
	}
	return hex.EncodeToString(h.Sum(nil)), size
}

// DigestBytes is Digest for a file.
func DigestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
