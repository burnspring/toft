// Package obs sets up process-wide logging.
//
// One stream, on stdout, in one format. That is the container convention and
// the only one every log agent already understands — the runtime captures
// stdout, and CloudWatch, Fluent Bit and Vector all read it from there. Writing
// files would mean rotation, a volume and a sidecar; writing the access log to
// a second stream would lose the interleaving that makes an incident readable
// and double the shipping configuration. Lines are told apart by a field
// instead: `kind=access` for the access line, `kind=app` and `kind=app_dropped`
// for a shared app's own output (internal/appout), everything else the
// platform's.
//
// This package deliberately does *not* export a logger. `slog`'s package-level
// functions are already used across `internal/events`, `mail`, `slack` and
// `appint`, and `Init` is what makes them structured; threading a `*slog.Logger`
// through every constructor to say the same thing would be a migration with no
// payoff.
package obs

import (
	"context"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel/trace"
)

// Init installs the default logger and returns the one thing worth handing
// around: the level, as a variable that can be set for the life of the process.
//
// Everything else about this is fixed at startup. `slog.SetDefault` re-points
// the standard `log` package at the same handler, so the existing `log.Printf`
// lines arrive in the same stream and the same format as the structured ones —
// which is why a 386-call-site migration is not a prerequisite for structured
// logs, and why the returned level reaches all 386 of them.
//
// **The level is settable and the format is not**, and they are not the same
// kind of thing. The format is a fact about who is reading the stream — `text`
// is a human at a terminal, `json` is a collector parsing it — decided once when
// the log destination was wired, and changing it mid-flight hands that collector
// a format change it did not ask for. The level is how much the platform says,
// which is a quantity, and it is the quantity an administrator most often wants
// to change and least often can: raising it to `debug` to diagnose a deploy used
// to mean restarting the platform, which on a busy install ends the very state
// being diagnosed.
//
// `slog.HandlerOptions.Level` takes a `slog.Leveler`, so a `*slog.LevelVar`
// makes this atomically settable with no handler rebuild and no lock on the
// write path. It reaches no workload at all, which is exactly why it is the one
// dial that may take effect immediately.
func Init(format string, level slog.Level) *slog.LevelVar {
	lv := new(slog.LevelVar)
	lv.Set(level)
	opts := &slog.HandlerOptions{Level: lv}

	var h slog.Handler
	if format == "json" {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	base = h
	slog.SetDefault(slog.New(&traceHandler{Handler: h}))
	return lv
}

// base is the handler Init installed, kept so Emit can hand it a record
// directly.
var base slog.Handler

// Emit writes one record through the handler Init installed — **the same
// instance**, so the same mutex, so one write per record and no interleaving
// with any other line at any length. A second handler on stdout would be a
// second mutex, and a pipe is atomic only up to 4 KiB.
//
// It is past the level gate on purpose: Handle does not consult the level,
// which is how much the platform says and has no hold on what an app said.
// Before Init (a test) it falls back to the default logger's handler.
func Emit(r slog.Record) error {
	h := base
	if h == nil {
		h = slog.Default().Handler()
	}
	return h.Handle(context.Background(), r)
}

// traceHandler stamps `trace_id` and `span_id` on every record that has a span
// in its context.
//
// This is what makes a log line and an exported span the same request rather
// than two accounts of it, and it is the reason adopting the OTLP logs signal
// later is a change of transport rather than of content. Records with no span
// — anything logged at startup, from a background sweep, or through the
// standard `log` package — simply carry neither field.
type traceHandler struct{ slog.Handler }

func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{Handler: h.Handler.WithGroup(name)}
}
