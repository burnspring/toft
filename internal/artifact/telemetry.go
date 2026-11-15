package artifact

import (
	"context"
	"time"

	"toft/internal/telemetry"
)

// Instrumented wraps a store so every operation is timed.
//
// A decorator rather than calls sprinkled through each backend, for the reason
// the redaction stage is a stage: there are two backends now and there will be
// more, and an instrumentation that has to be remembered in each one is an
// instrumentation that is eventually missing from one of them. The interface is
// the seam; this is what the seam is for.
//
// It wraps whether or not telemetry is on at boot, because it can be switched
// on from Admin → Telemetry afterwards; the record functions are no-ops while
// it is off. Every caller reaches the optional interfaces through PresignerOf,
// UploaderOf and DeleteAppRuns, which see through the wrapper — never a bare
// type assertion on the store.
func Instrumented(s Store, backend string) Store {
	if s == nil {
		return s
	}
	return instrumented{Store: s, backend: backend}
}

type instrumented struct {
	Store
	backend string
}

// The operation names. A closed set, because `operation` is a metric attribute
// and this is the whole of the interface.
const (
	opPut    = "put"
	opGet    = "get"
	opStat   = "stat"
	opDelete = "delete"
	opExpire = "expire"
)

func (i instrumented) Put(ctx context.Context, key, dir string) (Descriptor, error) {
	ctx, done := i.begin(ctx, opPut)
	d, err := i.Store.Put(ctx, key, dir)
	done(err)
	return d, err
}

func (i instrumented) Get(ctx context.Context, key, dest string) error {
	ctx, done := i.begin(ctx, opGet)
	err := i.Store.Get(ctx, key, dest)
	done(err)
	return err
}

func (i instrumented) Stat(ctx context.Context, key string) (Descriptor, error) {
	ctx, done := i.begin(ctx, opStat)
	d, err := i.Store.Stat(ctx, key)
	done(err)
	return d, err
}

func (i instrumented) Delete(ctx context.Context, key string) error {
	ctx, done := i.begin(ctx, opDelete)
	err := i.Store.Delete(ctx, key)
	done(err)
	return err
}

func (i instrumented) Expire(ctx context.Context, appID string, keep []string) (int, error) {
	ctx, done := i.begin(ctx, opExpire)
	n, err := i.Store.Expire(ctx, appID, keep)
	done(err)
	return n, err
}

// begin opens the span and returns the closer.
//
// The *key* is never an attribute. It contains an app id and a deployment id,
// which would be an unbounded label on a metric and is already carried by the
// span that encloses this one — an artifact operation happens inside a build,
// a restore or a prune, and those know whose it is.
func (i instrumented) begin(ctx context.Context, op string) (context.Context, func(error)) {
	start := time.Now()
	ctx, span := telemetry.StartClient(ctx, "artifact."+op)
	return ctx, func(err error) {
		span.End(err)
		telemetry.RecordArtifact(ctx, op, i.backend, telemetry.Outcome(err), time.Since(start))
	}
}
