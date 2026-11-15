package main

import (
	"bufio"
	"context"
	"io"
)

// **A read of stdin gives way to a signal.** The signal handler cancels the
// run's context, and a context cannot unblock a read: `sleep 1000 | timeout 5
// toft env set --from-file -` sat on io.ReadAll after SIGTERM and SIGINT
// alike, so only SIGKILL ended a stuck pipeline. Every read of stdin goes
// through these two, which read on a goroutine and return context.Canceled —
// the interrupted run's error — the moment the context ends. The goroutine is
// left blocked on the read; the process is about to exit.

// readAllCtx is io.ReadAll(r), unless ctx ends first.
func readAllCtx(ctx context.Context, r io.Reader) ([]byte, error) {
	type result struct {
		b   []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(r)
		done <- result{b, err}
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case res := <-done:
		return res.b, res.err
	case <-ctx.Done():
		return nil, interruptedRead(ctx)
	}
}

// readLineCtx is one line of r, its newline kept, unless ctx ends first.
func readLineCtx(ctx context.Context, r io.Reader) (string, error) {
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(r).ReadString('\n')
		done <- result{line, err}
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case res := <-done:
		return res.line, res.err
	case <-ctx.Done():
		return "", interruptedRead(ctx)
	}
}

// stdinInterrupted is a read of stdin a signal ended. It is context.Canceled to
// every caller that checks — they return it untouched — and report() says it,
// exit 1 code `interrupted`, because unlike a watch nothing was printed before
// the signal arrived (BB-016, third 2026-11-03 list).
type stdinInterrupted struct{ err error }

func (e *stdinInterrupted) Error() string {
	return "interrupted while reading stdin — nothing was sent"
}
func (e *stdinInterrupted) Unwrap() error { return e.err }

func interruptedRead(ctx context.Context) error {
	return &stdinInterrupted{err: ctx.Err()}
}
