package telemetry

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// allowedSpanAttributes is the complete set of keys a span may carry off this
// process. Anything else is dropped before the wire.
//
// This is an allow-list and a *stage*, not a convention followed at the call
// site, for two reasons. The call sites are dozens and will grow, while this
// list is one file. And it makes the privacy claim testable as a property —
// "no exported span ever carried a key outside this set" — rather than testable
// as a code review, which is not a test.
//
// Adding a key here is a deliberate act with a paragraph attached. See ADR §31
// D7 for what may never be added: passwords, tokens, API key material, cookies,
// Authorization headers, environment variable values, app storage contents,
// request or response bodies, build log contents, email addresses, Slack
// channel names, the X-Toft-Act-For blob and the X-Toft-Role
// assertion. The allow-list makes both of those unreachable rather than
// forbidden, which is the point of a positive list — but they are named here so
// that a future addition has to argue against a sentence.
var allowedSpanAttributes = map[attribute.Key]bool{
	// Semantic conventions, so a Toft span reads like every other span in
	// the operator's backend.
	"http.request.method":       true,
	"http.route":                true,
	"http.response.status_code": true,
	"url.scheme":                true,
	// url.path is the one raw-ish value allowed, and only on the platform
	// surface for a request that matched no route — which is exactly the case
	// where "what was it asking for" is the whole question. It is a span
	// attribute and never a metric attribute; the metric gets __unmatched__.
	"url.path":                 true,
	"server.address":           true,
	"server.port":              true,
	"network.protocol.version": true,
	"error.type":               true,
	"exception.type":           true,
	// The one free-text field in the pipeline, and therefore the one leak
	// vector. Truncated and scrubbed below.
	"exception.message":  true,
	"db.system.name":     true,
	"db.operation.name":  true,
	"code.function.name": true,

	// Toft's own. Every one is an opaque id or a value from a closed enum:
	// no slugs (renameable and human-chosen), no names, no addresses.
	"toft.app.id":           true,
	"toft.surface":          true,
	"toft.execution_mode":   true,
	"toft.release.id":       true,
	"toft.deployment.id":    true,
	"toft.job.run.id":       true,
	"toft.scheduled.run.id": true,
	"toft.build.phase":      true,
	"toft.framework":        true,
	"toft.outcome":          true,
	"toft.trigger":          true,
	// A sweep's own detail: what it removed, where it stopped, and the horizon
	// it removed against. All numbers and one timestamp — the *cutoff*, which
	// is derived from a retention setting and says nothing about any row that
	// was on the wrong side of it.
	"toft.sweep.cutoff":            true,
	"toft.sweep.truncated":         true,
	"toft.sweep.removed.rows":      true,
	"toft.sweep.removed.log_lines": true,
	"toft.sweep.removed.objects":   true,
	"toft.sweep.removed.releases":  true,
	"toft.skip_reason":             true,
	"toft.rollback":                true,
	"toft.provider.kind":           true,
}

// enduserIDKey is on the allow-list conditionally: it passes only when
// OTEL_RECORD_ENDUSER_ID is set. It is a UUID rather than a name or an address,
// but a trace backend is not covered by this platform's authorization model, so
// an operator has to choose. It is a *span* attribute only — never a metric
// attribute, at any setting, because that is the D5 rule and there is no
// override for it.
const enduserIDKey = attribute.Key("enduser.id")

// maxExceptionMessage bounds the free-text field. Long enough to carry a useful
// message, short enough that a dumped struct or a leaked body cannot ride out
// on one.
const maxExceptionMessage = 256

// redacting is the stage between the span queue and the exporter.
//
// It is an exporter wrapper rather than an sdktrace.SpanProcessor because a
// processor's OnEnd receives a ReadOnlySpan and cannot rewrite it; rewriting on
// the way to the wire is the only place the filter can actually bite. The
// effect is the one D7 specifies: every span that leaves this process has been
// through it, whatever call site produced it.
type redacting struct {
	next sdktrace.SpanExporter
}

func (r redacting) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		out[i] = redact(s)
	}
	return r.next.ExportSpans(ctx, out)
}

func (r redacting) Shutdown(ctx context.Context) error { return r.next.Shutdown(ctx) }

// redact returns a view of the span carrying only allow-listed attributes.
//
// It rebuilds unconditionally rather than detecting whether anything needed
// changing. A "did this span need redacting?" fast path is a branch that has to
// stay correct forever for a saving measured on the *export* path, which runs
// once per batch of 512 rather than once per request — and getting it wrong
// means a span goes out unfiltered.
func redact(s sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
	events := s.Events()
	filteredEvents := make([]sdktrace.Event, len(events))
	for i, e := range events {
		e.Attributes = filterAttributes(e.Attributes)
		filteredEvents[i] = e
	}
	return redactedSpan{
		ReadOnlySpan: s,
		attrs:        filterAttributes(s.Attributes()),
		events:       filteredEvents,
	}
}

func filterAttributes(in []attribute.KeyValue) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(in))
	for _, kv := range in {
		switch {
		case kv.Key == enduserIDKey:
			if recordEnduserID.Load() {
				out = append(out, validUTF8(kv))
			}
		case !allowedSpanAttributes[kv.Key]:
			// Dropped silently. A counter here would be a per-span write on the
			// export path for something that is a programming error, not an
			// operational condition — the test is what catches it.
		case kv.Key == "exception.message":
			out = append(out, attribute.String("exception.message", scrub(kv.Value.Emit())))
		default:
			out = append(out, validUTF8(kv))
		}
	}
	return out
}

// validUTF8 repairs a string attribute that is not valid UTF-8.
//
// OTLP is proto3, protobuf-go refuses to marshal a string field that is not
// UTF-8, and the exporter marshals a batch as one message — so one span
// carrying `/%ff` as its url.path would lose every span in its batch. The
// repair is here, in the stage every exported span passes through, because
// an error quoting a file name or an upstream reply can carry arbitrary bytes
// from anywhere.
func validUTF8(kv attribute.KeyValue) attribute.KeyValue {
	if kv.Value.Type() != attribute.STRING {
		return kv
	}
	if v := kv.Value.AsString(); !utf8.ValidString(v) {
		return attribute.String(string(kv.Key), strings.ToValidUTF8(v, "\uFFFD"))
	}
	return kv
}

// redactedSpan is a view of a span with its attributes replaced.
//
// sdktrace.ReadOnlySpan has an unexported method precisely so nobody implements
// it from outside; embedding the original satisfies that and inherits every
// field this does not override, which is also what keeps this struct correct
// when the SDK adds a method.
type redactedSpan struct {
	sdktrace.ReadOnlySpan
	attrs  []attribute.KeyValue
	events []sdktrace.Event
}

func (s redactedSpan) Attributes() []attribute.KeyValue { return s.attrs }
func (s redactedSpan) Events() []sdktrace.Event         { return s.events }

// --- the scrubber -----------------------------------------------------------

var (
	scrubMu      sync.RWMutex
	scrubLiteral []string
)

// secretPatterns are the shapes a secret takes when it ends up inside an error
// message. Every one of them has been observed in a real error string in this
// codebase's dependencies: a wrapped HTTP error carrying the request's
// Authorization header, a pgx dial error quoting the whole connection URL, a
// key-parsing error quoting the key.
var secretPatterns = []*regexp.Regexp{
	// Toft's credential prefixes: personal keys, deploy keys, the app-identity
	// token the SDKs hold, the exchanged key (toftx_), and the retired app
	// key, which a leaked old one still wears. A deploy key travels in X-API-Key as often as in a bearer
	// header, so the bearer pattern below does not cover it. The body is required to be hex, as
	// every one of them is minted: `toft_` is also the cookie prefix and the
	// metric prefix, and a looser body would redact `toft_session` and
	// `toft_build_info` out of the very log lines that are about them.
	regexp.MustCompile(`\b(toft|toftu|tofta|toftd|toftx)_[0-9a-f]{16,}`),
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._\-+/=]{8,}`),
	// A connection URL with credentials in it. The scheme list is deliberately
	// broad: what matters is `//user:pass@`, not which server it points at.
	regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.\-]*)://[^/\s:@]+:[^/\s@]+@`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
	// A JWT, which is what a leaked session cookie looks like in a log line.
	regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{4,}\.[A-Za-z0-9_\-]{4,}\.[A-Za-z0-9_\-]{4,}`),
}

const redacted = "[redacted]"

// newScrubber seeds the literal half of the scrubber with this install's actual
// secrets.
//
// Seeding from the live config rather than from a list written here is the
// point: a secret added to config.Config later, and not to this list, still
// gets scrubbed as long as the caller passes it — and the test asserts against
// the real values, so a *new* config secret that nobody passed is a visible gap
// rather than a silent one.
//
// Values shorter than eight bytes are ignored. A three-character "secret" would
// match inside ordinary words and turn every message into redaction confetti,
// which is a worse outcome than the leak it was guarding against.
func newScrubber(secrets []string) {
	var kept []string
	for _, s := range secrets {
		if len(s) >= 8 {
			kept = append(kept, s)
		}
	}
	scrubMu.Lock()
	scrubLiteral = kept
	scrubMu.Unlock()
}

// scrub removes secret-shaped text and truncates. Order matters: literals and
// patterns first, then the length cap, so a secret is not left half-visible by
// a truncation that cut it in two.
func scrub(s string) string {
	scrubMu.RLock()
	literals := scrubLiteral
	scrubMu.RUnlock()

	for _, secret := range literals {
		if strings.Contains(s, secret) {
			s = strings.ReplaceAll(s, secret, redacted)
		}
	}
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, redacted)
	}
	// Valid UTF-8 first, and cut on a rune boundary: a cut through a
	// multi-byte rune would make the scrubber itself the source of the bytes
	// the exporter cannot marshal.
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) > maxExceptionMessage {
		n := maxExceptionMessage
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = s[:n] + "…"
	}
	return s
}
